package runner

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Installation tokens last an hour. A run that outlives the token it was assigned with must still push:
// the runner asks for a fresh token at push time, and keeps retrying if the first one is refused.
func TestPushUsesFreshTokenAfterAssignedExpires(t *testing.T) {
	ctx := context.Background()
	pushRetryDelay = time.Millisecond
	root := t.TempDir()
	bare := filepath.Join(root, "o.git")
	wt := filepath.Join(root, "wt")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, bare, "config", "http.receivepack", "true")
	git(t, root, "clone", "-q", bare, wt)
	os.WriteFile(filepath.Join(wt, "a"), []byte("1"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-qm", "work")

	valid := "fresh-token"
	backend, _ := exec.LookPath("git")
	h := &cgi.Handler{Path: backend, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=tester"}}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "basic "); !ok || got != base64.StdEncoding.EncodeToString([]byte("x-access-token:"+valid)) {
			rw.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(rw, "Invalid username or token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(rw, r)
	}))
	defer srv.Close()
	git(t, wt, "remote", "set-url", "origin", srv.URL+"/o.git")

	mints := 0
	w := &Worker{MintToken: func(context.Context, int64) (string, error) {
		mints++
		if mints == 1 {
			return "also-expired", nil // a stale first answer: the retry must re-mint
		}
		return valid, nil
	}}
	s := &session{w: w, a: proto.Assignment{RunID: 7, Branch: "wrangl/x-1", GitToken: "expired-assignment-token"}, id: agentIdentity{home: root, username: "t"}, dir: wt}
	if out, err := w.pushRun(ctx, s, "wrangl/x-1"); err != nil {
		t.Fatalf("push with expired assignment token: %v: %s", err, out)
	}
	if got, want := git(t, bare, "rev-parse", "refs/heads/wrangl/x-1"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s, want %s", got, want)
	}
}
