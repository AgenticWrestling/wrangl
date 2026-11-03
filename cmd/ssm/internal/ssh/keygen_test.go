// cmd/ssm/internal/ssh/keygen_test.go
package ssh_test

import (
	"os"
	"path/filepath"
	"testing"

	sshpkg "github.com/agenticwrestling/wrangl/cmd/ssm/internal/ssh"
)

func TestEnsureKeyPair_creates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pub, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(pub) == 0 {
		t.Fatal("expected non-empty public key")
	}
	if _, err := os.Stat(filepath.Join(dir, ".ssh", "id_ed25519")); err != nil {
		t.Fatalf("private key not created: %v", err)
	}
}

func TestEnsureKeyPair_idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pub1, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pub2, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if string(pub1) != string(pub2) {
		t.Fatal("second call returned different key — not idempotent")
	}
}
