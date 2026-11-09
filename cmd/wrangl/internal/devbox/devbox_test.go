package devbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/devbox"
)

func home(t *testing.T) string {
	d := t.TempDir()
	t.Setenv("HOME", d)
	return d
}

func block(id string) devbox.HostBlock {
	return devbox.HostBlock{Name: "myhost", InstanceID: id, Region: "us-west-2", Profile: "dev", User: "ec2-user", Wrangl: "/opt/my bin/wrangl"}
}

func TestEnsureKeyPair(t *testing.T) {
	d := home(t)
	os.MkdirAll(filepath.Join(d, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(d, ".ssh", "id_ed25519"), []byte("personal"), 0o600) // never touched
	a, err := devbox.EnsureKeyPair()
	if err != nil || !strings.HasPrefix(string(a), "ssh-ed25519 ") {
		t.Fatal(string(a), err)
	}
	b, _ := devbox.EnsureKeyPair()
	if string(a) != string(b) {
		t.Fatal("not idempotent")
	}
	if got, _ := os.ReadFile(filepath.Join(d, ".ssh", "id_ed25519")); string(got) != "personal" {
		t.Fatal("the user's own key was modified")
	}
	if st, _ := os.Stat(filepath.Join(d, ".ssh", devbox.KeyName)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	os.Remove(filepath.Join(d, ".ssh", devbox.KeyName+".pub"))
	if _, err := devbox.EnsureKeyPair(); err == nil {
		t.Fatal("want error for a missing public key")
	}
}

func TestUpsertHostBlock(t *testing.T) {
	d := home(t)
	if err := devbox.UpsertHostBlock(block("i-OLD")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, ".ssh", "config")
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{"Host myhost", "HostName i-OLD", "wrangl_dev_ed25519", `ProxyCommand "/opt/my bin/wrangl" dev proxy %h --user %r --region us-west-2 --profile dev`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// unmanaged lines survive an update; other hosts are untouched
	amended := "Host other\n  HostName x\n" + strings.Replace(s, "Host myhost\n", "Host myhost\n  LocalForward 8080 localhost:8080\n", 1)
	os.WriteFile(p, []byte(amended), 0o600)
	devbox.UpsertHostBlock(block("i-NEW"))
	data, _ = os.ReadFile(p)
	s = string(data)
	if strings.Contains(s, "i-OLD") || !strings.Contains(s, "i-NEW") || !strings.Contains(s, "LocalForward 8080 localhost:8080") || !strings.Contains(s, "Host other\n  HostName x") {
		t.Fatalf("bad update:\n%s", s)
	}
	if strings.Count(s, "Host myhost") != 1 {
		t.Fatalf("duplicate block:\n%s", s)
	}
}

func TestLookupHostReadsLegacyMarkers(t *testing.T) {
	d := home(t)
	os.MkdirAll(filepath.Join(d, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(d, ".ssh", "config"), []byte("Host old\n  # ssm-region: eu-west-1\n  # ssm-profile: prod\n  HostName i-1\n  User bob\n"), 0o600)
	m := devbox.LookupHost("old")
	if m.Region != "eu-west-1" || m.Profile != "prod" || m.User != "bob" || m.InstanceID != "i-1" {
		t.Fatalf("%+v", m)
	}
	devbox.UpsertHostBlock(devbox.HostBlock{Name: "old", InstanceID: "i-1", Region: "eu-west-1", Profile: "prod", User: "bob"})
	if m := devbox.LookupHost("old"); m.Region != "eu-west-1" || m.Profile != "prod" {
		t.Fatalf("after upsert %+v", m)
	}
	data, _ := os.ReadFile(filepath.Join(d, ".ssh", "config"))
	if strings.Contains(string(data), "ssm-region") {
		t.Fatal("legacy markers must be replaced")
	}
	if (devbox.LookupHost("none") != devbox.HostMeta{}) {
		t.Fatal("unknown host must be zero")
	}
}
