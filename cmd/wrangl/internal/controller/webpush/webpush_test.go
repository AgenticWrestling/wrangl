package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func TestSendAndPruneGoneSubscriptions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, _ := db.Open(ctx, filepath.Join(dir, "w.db"))
	defer d.Close()
	keys, err := EnsureKeys(filepath.Join(dir, "secrets"))
	if err != nil || keys.Public == "" {
		t.Fatal(err)
	}
	again, _ := EnsureKeys(filepath.Join(dir, "secrets"))
	if again != keys {
		t.Fatal("VAPID keys regenerated")
	}
	var got atomic.Int32
	var vapid atomic.Value
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		vapid.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Content-Encoding") != "aes128gcm" {
			http.Error(w, "not encrypted", 400)
			return
		}
		w.WriteHeader(201)
	}))
	defer live.Close()
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(410) }))
	defer gone.Close()

	// A browser's subscription keys: P-256 public key + 16-byte auth secret.
	browser, _ := ecdh.P256().GenerateKey(rand.Reader)
	p256dh := base64.RawURLEncoding.EncodeToString(browser.PublicKey().Bytes())
	auth := make([]byte, 16)
	rand.Read(auth)
	a := base64.RawURLEncoding.EncodeToString(auth)
	s := &Service{DB: d, Keys: keys, Subject: "mailto:owner@example.com", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, ep := range []string{live.URL + "/sub", gone.URL + "/sub"} {
		if err := s.Subscribe(ctx, ep, p256dh, a, "test"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Send(ctx, Notification{Title: "Question", Body: "Which colour?", URL: "/inbox"})
	if n != 1 || err != nil || got.Load() != 1 {
		t.Fatalf("sent %d (%v), live got %d", n, err, got.Load())
	}
	if h, _ := vapid.Load().(string); !strings.HasPrefix(h, "vapid t=") {
		t.Fatalf("no VAPID auth: %q", h)
	}
	subs, _ := d.RQ.ListPushSubscriptions(ctx)
	if len(subs) != 1 || !strings.HasPrefix(subs[0].Endpoint, live.URL) {
		t.Fatalf("gone subscription not pruned: %+v", subs)
	}
}
