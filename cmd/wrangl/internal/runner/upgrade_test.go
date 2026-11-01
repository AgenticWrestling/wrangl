package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfUpgradeFetch(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho new wrangl\n")
	sum := sha256.Sum256(newBin)
	want := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dl/wrangl-linux-x86_64" {
			http.NotFound(w, r)
			return
		}
		w.Write(newBin)
	}))
	defer srv.Close()
	dir := t.TempDir()
	bin := filepath.Join(dir, "wrangl")
	os.WriteFile(bin, []byte("old"), 0o755)
	u := &SelfUpgrade{BinaryPath: bin}
	ctx := context.Background()

	unchanged := func() {
		t.Helper()
		if b, _ := os.ReadFile(bin); string(b) != "old" {
			t.Fatalf("binary changed after a failed upgrade: %q", b)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 1 {
			t.Fatalf("temp files left behind: %v", ents)
		}
	}
	if err := u.Fetch(ctx, srv.URL, "x86_64", strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("wrong sha accepted: %v", err)
	}
	unchanged()
	if err := u.Fetch(ctx, srv.URL, "aarch64", want); err == nil {
		t.Fatal("404 accepted")
	}
	unchanged()
	if err := u.Fetch(ctx, srv.URL, "x86_64", "not-a-sha"); err == nil {
		t.Fatal("malformed sha accepted")
	}
	if got, _ := u.Sum(); got == want {
		t.Fatal("sum of old binary equals new")
	}
	if err := u.Fetch(ctx, srv.URL+"/", "x86_64", want); err != nil {
		t.Fatal(err)
	}
	if got, _ := u.Sum(); got != want {
		t.Fatalf("sum after upgrade %s", got)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
