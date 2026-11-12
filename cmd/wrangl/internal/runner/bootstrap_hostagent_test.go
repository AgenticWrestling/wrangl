package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func testJWT(t *testing.T, pub string) string {
	t.Helper()
	akp, _ := nkeys.CreateAccount()
	tok, err := jwt.NewUserClaims(pub).Encode(akp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func fakeEnrol(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.EnrollRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(proto.EnrollResponse{RunnerID: 3, Name: "host", UserJWT: testJWT(t, req.NKeyPublic), NATSURLs: []string{"tls://x:4222"}, NATSCA: "CA"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestBootstrapHostAgentUser(t *testing.T) {
	root := t.TempDir()
	cfg, err := Bootstrap(context.Background(), BootstrapOptions{
		Controller: fakeEnrol(t), Token: "t", Root: root, IMDSURL: "http://127.0.0.1:1",
		NoSystemd: true, NoUser: true, Host: true, HostAgentUser: "wrangl-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Host || cfg.AgentUser != "wrangl-agent" || len(cfg.PoweroffCommand) != 0 {
		t.Fatalf("config %+v", cfg)
	}
	if !strings.HasPrefix(cfg.StateDir, root) || !strings.Contains(cfg.StateDir, "wrangl-host-runner") || !strings.HasPrefix(cfg.CredsFile, root) {
		t.Errorf("paths outside the prefix or not host-specific: %+v", cfg)
	}
	if _, err := LoadConfig(filepath.Join(root, "etc/wrangl-host-runner/runner.yaml")); err != nil {
		t.Fatal(err)
	}
	// Worktrees live under the agent's home, not the owner's.
	a := &Applier{Cfg: cfg}
	cfg.AgentUser = "" // lookup of the (absent) user is the one thing a unit test can't do
	id, _ := a.agent()
	if cfg.AgentHome == "" || !strings.HasPrefix(id.home, root) {
		t.Errorf("agent home %q", id.home)
	}
	cu, _ := user.Current()
	if cfg.AgentHome == cu.HomeDir {
		t.Errorf("agent home is the owner's home")
	}
}

func TestBootstrapAgentUserValidation(t *testing.T) {
	for _, o := range []BootstrapOptions{
		{Host: true, HostAgentUser: "bad name"},
		{Host: false, HostAgentUser: "wrangl-agent"},
	} {
		o.Controller, o.Token, o.Root, o.NoSystemd, o.NoUser = "http://x", "t", t.TempDir(), true, true
		if _, err := Bootstrap(context.Background(), o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestEnsureAgentUserIdempotentForExisting(t *testing.T) {
	cu, _ := user.Current()
	if err := ensureAgentUser(cu.Username); err != nil { // exists: must not call useradd
		t.Fatal(err)
	}
}
