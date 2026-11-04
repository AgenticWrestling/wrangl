package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
)

// enrolled writes runner files for a runner issued by e, as bootstrap would.
func enrolled(t *testing.T, e *natsd.Embedded, id int64) *Config {
	t.Helper()
	dir := t.TempDir()
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	userJWT, err := e.Keys.IssueRunner(id, pub)
	if err != nil {
		t.Fatal(err)
	}
	creds, _ := jwt.FormatUserConfig(userJWT, seed)
	os.WriteFile(filepath.Join(dir, "runner.creds"), creds, 0o600)
	os.WriteFile(filepath.Join(dir, "ca.pem"), e.CA.CertPEM, 0o644)
	return &Config{RunnerID: id, Name: "r", NATSURLs: []string{e.URL()},
		CAFile: filepath.Join(dir, "ca.pem"), CredsFile: filepath.Join(dir, "runner.creds")}
}

func TestDaemonHeartbeatsAndAnswersPing(t *testing.T) {
	e, err := natsd.Start(t.TempDir(), "127.0.0.1:-1")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cfg := enrolled(t, e, 5)
	hb, _ := e.Conn.SubscribeSync(natsd.HeartbeatWildcard)
	e.Conn.Flush()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, cfg, DaemonOptions{HeartbeatInterval: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m, err := hb.NextMsg(5 * time.Second)
	if err != nil || m.Subject != "wr.r.5.hb" {
		t.Fatalf("heartbeat: %v %v", m, err)
	}
	var resp *nats.Msg
	for i := 0; i < 50; i++ { // wait for the ping subscription
		if resp, err = e.Conn.Request(natsd.RunnerCmdSubject(5, "ping"), nil, 200*time.Millisecond); err == nil {
			break
		}
	}
	if err != nil || len(resp.Data) == 0 {
		t.Fatalf("ping: %v", err)
	}
}

func TestOrphanedDaemonPowersOff(t *testing.T) {
	e, err := natsd.Start(t.TempDir(), "127.0.0.1:-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := enrolled(t, e, 6)
	var powered atomic.Int32
	done := make(chan struct{})
	go func() {
		Run(context.Background(), cfg, DaemonOptions{
			HeartbeatInterval: 20 * time.Millisecond, OrphanAfter: 300 * time.Millisecond,
			Poweroff: func() error { powered.Add(1); return nil },
			Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	if powered.Load() != 0 {
		t.Fatal("powered off while connected")
	}
	e.Close() // controller gone
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not power off after losing the controller")
	}
	if powered.Load() != 1 {
		t.Fatalf("poweroff called %d times", powered.Load())
	}
}
