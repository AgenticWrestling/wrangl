// Package webpush sends Web Push notifications to the owner's installed PWA (spec §11):
// questions, failed or blocked runs, runners going offline.
package webpush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	wp "github.com/SherClockHolmes/webpush-go"

	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
)

// Keys is the VAPID key pair (secrets/vapid.json).
type Keys struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

// EnsureKeys loads secrets/vapid.json, generating it once.
func EnsureKeys(secretsDir string) (Keys, error) {
	p := filepath.Join(secretsDir, "vapid.json")
	var k Keys
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		priv, pub, err := wp.GenerateVAPIDKeys()
		if err != nil {
			return k, err
		}
		k = Keys{Public: pub, Private: priv}
		b, _ = json.Marshal(k)
		if err := os.MkdirAll(secretsDir, 0o700); err != nil {
			return k, err
		}
		return k, os.WriteFile(p, b, 0o600)
	} else if err != nil {
		return k, err
	}
	return k, json.Unmarshal(b, &k)
}

// Service sends notifications to every subscription.
type Service struct {
	DB      *db.DB
	Keys    Keys
	Subject string // mailto: or https: contact for the push service
	HTTP    *http.Client
	Log     *slog.Logger
}

// Notification is what the service worker shows.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`           // opened on click
	Tag   string `json:"tag,omitempty"` // collapses repeats
	// Actions are buttons (Android); each posts to its URL from the service worker.
	Actions []Action `json:"actions,omitempty"`
}

// Action is a notification button.
type Action struct {
	Action string `json:"action"`
	Title  string `json:"title"`
	URL    string `json:"url"` // POSTed by the service worker
}

// Subscribe stores (or refreshes) a browser's push subscription.
func (s *Service) Subscribe(ctx context.Context, endpoint, p256dh, auth, ua string) error {
	if endpoint == "" || p256dh == "" || auth == "" {
		return errors.New("subscription needs endpoint and keys")
	}
	return s.DB.Q.UpsertPushSubscription(ctx, dbgen.UpsertPushSubscriptionParams{Endpoint: endpoint, P256dh: p256dh, Auth: auth, UserAgent: ua})
}

// Send delivers n to every subscription; gone subscriptions (404/410) are removed. It
// returns how many deliveries succeeded.
func (s *Service) Send(ctx context.Context, n Notification) (int, error) {
	subs, err := s.DB.RQ.ListPushSubscriptions(ctx)
	if err != nil {
		return 0, err
	}
	payload, _ := json.Marshal(n)
	opts := &wp.Options{Subscriber: s.Subject, VAPIDPublicKey: s.Keys.Public, VAPIDPrivateKey: s.Keys.Private, TTL: 3600,
		Urgency: wp.UrgencyHigh, Topic: n.Tag}
	if s.HTTP != nil { // a nil *http.Client in the interface field would be a non-nil interface
		opts.HTTPClient = s.HTTP
	}
	ok := 0
	var errs []error
	for _, sub := range subs {
		resp, err := wp.SendNotificationWithContext(ctx, payload, &wp.Subscription{Endpoint: sub.Endpoint, Keys: wp.Keys{P256dh: sub.P256dh, Auth: sub.Auth}}, opts)
		if err != nil {
			s.DB.Q.PushFailed(ctx, sub.ID)
			errs = append(errs, err)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound:
			s.DB.Q.DeletePushSubscription(ctx, sub.Endpoint) // the browser unsubscribed
		case resp.StatusCode >= 300:
			s.DB.Q.PushFailed(ctx, sub.ID)
			errs = append(errs, fmt.Errorf("push service: HTTP %d", resp.StatusCode))
		default:
			ok++
			s.DB.Q.PushSucceeded(ctx, dbgen.PushSucceededParams{LastSuccessAt: sqlNow(), ID: sub.ID})
		}
	}
	return ok, errors.Join(errs...)
}

// Notify sends in the background and logs failures (callers are event handlers).
func (s *Service) Notify(n Notification) {
	if s == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.Send(ctx, n); err != nil {
			s.Log.Warn("web push", "title", n.Title, "err", err)
		}
	}()
}
