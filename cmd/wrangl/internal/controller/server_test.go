package controller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
)

func TestServeHealthzStreamsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	if _, err := Init(ctx, dir, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = dir
	cfg.Listen.HTTP = "127.0.0.1:0"
	cfg.Listen.NATS = "127.0.0.1:-1"
	cfg.Listen.Enroll = "127.0.0.1:0"
	cfg.Listen.Unix = filepath.Join(dir, "api.sock")

	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-s.Ready():
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("not ready")
	}

	want := map[string]string{"db": "ok", "nats": "ok", "jetstream": "ok"}
	check := func(name string, c *http.Client, url string) {
		resp, err := c.Get(url)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var got map[string]string
		json.Unmarshal(b, &got)
		if resp.StatusCode != 200 || len(got) != 3 || got["db"] != want["db"] || got["nats"] != "ok" || got["jetstream"] != "ok" {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	check("tcp", http.DefaultClient, "http://"+s.HTTPAddr()+"/healthz")
	unixClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	check("unix", unixClient, "http://wrangl/healthz")
	if fi, err := os.Stat(cfg.Listen.Unix); err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}

	// Anonymous / plaintext NATS is refused now that operator mode and TLS are on.
	if nc, err := nats.Connect(strings.Replace(s.NATSURL(), "tls://", "nats://", 1), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("anonymous plaintext NATS connection accepted")
	}
	// A runner with issued credentials, pinning the CA, can publish into WR_EVT.
	kp, _ := nkeys.CreateUser()
	pubKey, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	userJWT, err := s.Fleet.Keys.IssueRunner(9, pubKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, _ := natsd.ClientTLS(s.NATSCA())
	nc, err := nats.Connect(s.NATSURL(), nats.UserJWTAndSeed(userJWT, string(seed)), nats.Secure(tlsCfg),
		nats.CustomInboxPrefix(natsd.RunnerInboxPrefix(9))) // required: runners may only use their own inbox
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	if ack, err := js.Publish(ctx, "wr.evt.9.x1.log", []byte("hi")); err != nil || ack.Stream != StreamEvents {
		t.Fatalf("publish into WR_EVT: %v %v", ack, err)
	}
	nc.Close()

	// A second controller on the same socket must refuse rather than steal it.
	if _, err := listenUnix(cfg.Listen.Unix); err == nil {
		t.Fatal("second listener on live socket accepted")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown took longer than 5s")
	}
	if _, err := os.Stat(cfg.Listen.Unix); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}

func TestServeReclaimsStaleSocket(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api.sock")
	os.WriteFile(p, nil, 0o600) // leftover from an unclean exit
	l, err := listenUnix(p)
	if err != nil {
		t.Fatalf("stale socket not reclaimed: %v", err)
	}
	l.Close()
}
