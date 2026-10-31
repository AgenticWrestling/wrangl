package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestControllerDefaultsFillMissingKeys(t *testing.T) {
	c, err := LoadController(write(t, "data_dir: /srv/w\nlisten:\n  nats: 10.0.1.5:4222\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/srv/w" || c.Listen.NATS != "10.0.1.5:4222" {
		t.Fatalf("explicit values lost: %+v", c)
	}
	if c.Listen.HTTP != "127.0.0.1:8080" || c.Listen.Unix != "/run/wrangl/api.sock" || c.Listen.Enroll != "127.0.0.1:4223" {
		t.Fatalf("defaults not applied: %+v", c.Listen)
	}
}

func TestControllerEmptyFileIsDefaults(t *testing.T) {
	c, err := LoadController(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != DefaultDataDir {
		t.Fatalf("got %q", c.DataDir)
	}
}

func TestControllerRejectsUnknownKeys(t *testing.T) {
	_, err := LoadController(write(t, "data_dri: /typo\n"))
	if err == nil || !strings.Contains(err.Error(), "data_dri") {
		t.Fatalf("want unknown-key error, got %v", err)
	}
}

func TestControllerMissingFileErrors(t *testing.T) {
	if _, err := LoadController(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want error")
	}
}

func TestPathResolvesUnderDataDir(t *testing.T) {
	c := &Controller{DataDir: "/var/lib/wrangl"}
	if got := c.Path("secrets/gh.pem"); got != "/var/lib/wrangl/secrets/gh.pem" {
		t.Fatal(got)
	}
	if got := c.Path("/etc/key.pem"); got != "/etc/key.pem" {
		t.Fatal(got)
	}
}

func TestClientMissingFileIsDefault(t *testing.T) {
	c, err := LoadClient(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || c.Controller != DefaultLocalController {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestClientRoundTripAndRedact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	in := &Client{Controller: "https://w.example.com", CFServiceToken: &CFServiceToken{ID: "id", Secret: "s3cret"}}
	if err := SaveClient(p, in); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	out, err := LoadClient(p)
	if err != nil || out.Controller != in.Controller || out.CFServiceToken.Secret != "s3cret" {
		t.Fatalf("%+v %v", out, err)
	}
	if r := out.Redacted(); r.CFServiceToken.Secret != "REDACTED" || out.CFServiceToken.Secret != "s3cret" {
		t.Fatal("redaction wrong or mutated original")
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Setenv("WRANGL_X", "env")
	if Resolve("flag", "WRANGL_X", "file", "def") != "flag" {
		t.Fatal("flag")
	}
	if Resolve("", "WRANGL_X", "file", "def") != "env" {
		t.Fatal("env")
	}
	t.Setenv("WRANGL_X", "")
	if Resolve("", "WRANGL_X", "file", "def") != "file" {
		t.Fatal("file")
	}
	if Resolve("", "WRANGL_X", "", "def") != "def" {
		t.Fatal("def")
	}
}
