package natsd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func start(t *testing.T) (*Embedded, string) {
	t.Helper()
	dir := t.TempDir()
	e, err := Start(dir, "127.0.0.1:-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e, dir
}

// runnerConn enrols runner r the way a real runner does: it generates its own key, the
// controller signs a JWT for the public half, and it connects over TLS pinning the CA.
func runnerConn(t *testing.T, e *Embedded, r int64) *nats.Conn {
	t.Helper()
	kp, _ := nkeys.CreateUser()
	pubKey, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	userJWT, err := e.Keys.IssueRunner(r, pubKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := ClientTLS(e.CA.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	nc, err := nats.Connect(e.URL(),
		nats.UserJWTAndSeed(userJWT, string(seed)),
		nats.Secure(tlsCfg),
		nats.CustomInboxPrefix(RunnerInboxPrefix(r)),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err }),
	)
	if err != nil {
		t.Fatalf("runner %d connect: %v", r, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestKeysPersistAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	k1, err := EnsureKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := EnsureKeys(dir)
	if pub(k1.Account) != pub(k2.Account) || pub(k1.AccountSigning) != pub(k2.AccountSigning) {
		t.Fatal("keys regenerated on second load")
	}
	for _, f := range []string{"operator.nk", "sys.nk", "account.nk", "account-signing.nk"} {
		if fi, _ := os.Stat(filepath.Join(dir, f)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, fi.Mode())
		}
	}
}

func TestRunnerCanPublishOwnEventsIntoJetStream(t *testing.T) {
	e, _ := start(t)
	ctx := context.Background()
	cjs, _ := jetstream.New(e.Conn) // controller, in-process
	if _, err := cjs.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: "WR_EVT", Subjects: []string{"wr.evt.>"}}); err != nil {
		t.Fatalf("controller cannot use JetStream in operator mode: %v", err)
	}
	rjs, _ := jetstream.New(runnerConn(t, e, 7), jetstream.WithDefaultTimeout(3*time.Second))
	ack, err := rjs.Publish(ctx, "wr.evt.7.x1.log", []byte("hello"))
	if err != nil || ack.Stream != "WR_EVT" {
		t.Fatalf("runner publish with ack: %+v %v", ack, err)
	}
}

func TestRunnerIsolation(t *testing.T) {
	e, _ := start(t)
	r7 := runnerConn(t, e, 7)

	// Allowed: own command subject.
	if _, err := r7.SubscribeSync("wr.r.7.cmd.>"); err != nil {
		t.Fatal(err)
	}
	r7.Flush()
	if err := r7.LastError(); err != nil {
		t.Fatalf("own cmd subscribe rejected: %v", err)
	}

	// Denied: another runner's commands. The server reports a permissions violation
	// asynchronously, which surfaces as LastError after a round trip.
	r7b := runnerConn(t, e, 7)
	r7b.SubscribeSync("wr.r.8.cmd.>")
	r7b.Flush()
	time.Sleep(100 * time.Millisecond)
	if err := r7b.LastError(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
		t.Fatalf("subscribe to runner 8 cmds: want permissions violation, got %v", err)
	}

	// Denied: publishing another runner's events. Prove it by checking the controller
	// never receives the message.
	sub, _ := e.Conn.SubscribeSync("wr.evt.>")
	e.Conn.Flush()
	r7c := runnerConn(t, e, 7)
	r7c.Publish("wr.evt.8.x.log", []byte("forged"))
	r7c.Publish("wr.evt.7.x.log", []byte("legit"))
	r7c.Flush()
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil || m.Subject != "wr.evt.7.x.log" {
		t.Fatalf("first delivered: %v %v", m, err)
	}
	if m, err := sub.NextMsg(300 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("forged event was delivered: %v", m)
	}
}

func TestControllerRequestRunnerReplies(t *testing.T) {
	e, _ := start(t)
	r := runnerConn(t, e, 3)
	if _, err := r.Subscribe(RunnerCmdSubject(3, "ping"), func(m *nats.Msg) { m.Respond([]byte("pong")) }); err != nil {
		t.Fatal(err)
	}
	r.Flush()
	resp, err := e.Conn.Request(RunnerCmdSubject(3, "ping"), nil, 2*time.Second)
	if err != nil || string(resp.Data) != "pong" {
		t.Fatalf("request/reply: %v %v", resp, err)
	}
}

func TestTLSRequiredAndPinned(t *testing.T) {
	e, _ := start(t)
	kp, _ := nkeys.CreateUser()
	pubKey, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	userJWT, _ := e.Keys.IssueRunner(1, pubKey)

	// Plaintext is refused.
	plainURL := strings.Replace(e.URL(), "tls://", "nats://", 1)
	if nc, err := nats.Connect(plainURL, nats.UserJWTAndSeed(userJWT, string(seed)), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("plaintext connection accepted")
	}
	// A different CA is refused.
	other, _ := EnsureCA(t.TempDir())
	otherTLS, _ := ClientTLS(other.CertPEM)
	if nc, err := nats.Connect(e.URL(), nats.UserJWTAndSeed(userJWT, string(seed)), nats.Secure(otherTLS), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("connection trusting a foreign CA succeeded")
	}
}

func TestUnsignedOrForeignJWTRejected(t *testing.T) {
	e, _ := start(t)
	other, _ := EnsureKeys(t.TempDir()) // a different controller's keys
	kp, _ := nkeys.CreateUser()
	pubKey, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	foreign, _ := other.IssueRunner(1, pubKey)
	tlsCfg, _ := ClientTLS(e.CA.CertPEM)
	if nc, err := nats.Connect(e.URL(), nats.UserJWTAndSeed(foreign, string(seed)), nats.Secure(tlsCfg), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("JWT from another controller accepted")
	}
	if _, err := e.Keys.IssueRunner(1, "not-a-key"); err == nil {
		t.Fatal("issued a JWT for a bad public key")
	}
}
