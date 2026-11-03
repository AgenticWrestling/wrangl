package ghsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExchange(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/app-manifests/abc/conversions" {
			http.Error(w, "no", 404)
			return
		}
		w.Write([]byte(`{"id":42,"slug":"wrangl-author-x","html_url":"https://github.com/apps/wrangl-author-x","pem":"-----BEGIN RSA PRIVATE KEY-----\nk\n"}`))
	}))
	defer gh.Close()
	dir := t.TempDir()
	s := &Service{DataDir: dir, PublicURL: "https://ctrl.example.com", API: gh.URL}

	if _, err := s.Exchange(context.Background(), "abc", "bogus"); err == nil {
		t.Fatal("unknown state accepted")
	}
	st := s.NewState()
	app, err := s.Exchange(context.Background(), "abc", st)
	if err != nil || app.ID != 42 || app.Slug != "wrangl-author-x" {
		t.Fatalf("exchange: %+v %v", app, err)
	}
	if _, err := s.Exchange(context.Background(), "abc", st); err == nil {
		t.Fatal("state reused")
	}
	fi, err := os.Stat(filepath.Join(dir, KeyRel))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if got, ok := Load(dir); !ok || got.ID != 42 {
		t.Fatalf("load: %+v %v", got, ok)
	}
	if !strings.HasSuffix(app.InstallURL(), "/wrangl-author-x/installations/new") {
		t.Fatal(app.InstallURL())
	}
	m, err := s.Manifest("n")
	if err != nil || !strings.Contains(string(m), "https://ctrl.example.com/setup/github/callback") {
		t.Fatalf("manifest: %s %v", m, err)
	}
}
