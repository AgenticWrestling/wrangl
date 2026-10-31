package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
)

func TestInitIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	cfgPath := filepath.Join(t.TempDir(), "etc", "controller.yaml")

	res, err := Init(ctx, dir, cfgPath)
	if err != nil || !res.WroteConfig {
		t.Fatalf("%+v %v", res, err)
	}
	for sub, mode := range map[string]os.FileMode{"": 0o750, "bin": 0o750, "nats": 0o750, "secrets": 0o700} {
		fi, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%q: %v %v", sub, fi.Mode(), err)
		}
	}
	c, err := config.LoadController(cfgPath)
	if err != nil || c.DataDir != dir {
		t.Fatalf("written config does not load back: %+v %v", c, err)
	}
	alpha1 := alphabet(t, dir)

	// Hand-edit the config; a second init must not overwrite it or the alphabet.
	os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\nweb:\n  public_url: https://x\n"), 0o640)
	res, err = Init(ctx, dir, cfgPath)
	if err != nil || res.WroteConfig {
		t.Fatalf("second init: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), "https://x") {
		t.Fatal("config overwritten")
	}
	if alphabet(t, dir) != alpha1 {
		t.Fatal("alphabet changed on re-init")
	}
}

func alphabet(t *testing.T, dir string) string {
	t.Helper()
	d, err := db.Open(context.Background(), DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, err := d.RQ.GetMeta(context.Background(), MetaSqidsAlphabet)
	if err != nil || len(a) != 62 {
		t.Fatalf("alphabet %q %v", a, err)
	}
	return a
}

func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := SecretSet(dir, "claude_oauth_token", strings.NewReader("tok-123\n")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "secrets", "claude_oauth_token"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if v, _ := SecretGet(dir, "claude_oauth_token"); string(v) != "tok-123" {
		t.Fatalf("value %q", v)
	}
	SecretSet(dir, "gh-webhook", strings.NewReader("w"))
	if names, _ := SecretList(dir); strings.Join(names, ",") != "claude_oauth_token,gh-webhook" {
		t.Fatalf("list %v", names)
	}
	for _, bad := range []string{"../etc", "A", "a/b", "", ".."} {
		if err := SecretSet(dir, bad, strings.NewReader("x")); err == nil {
			t.Fatalf("accepted name %q", bad)
		}
	}
	if err := SecretSet(dir, "empty", strings.NewReader("\n")); err == nil {
		t.Fatal("accepted empty value")
	}
	if err := SecretRm(dir, "gh-webhook"); err != nil {
		t.Fatal(err)
	}
	if names, _ := SecretList(dir); len(names) != 1 {
		t.Fatalf("after rm %v", names)
	}
}
