// cmd/ssm/internal/ssh/config_test.go
package ssh_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sshpkg "github.com/nreid/devhost/cmd/ssm/internal/ssh"
)

func TestUpsertHostBlock_creates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.MkdirAll(filepath.Join(dir, ".ssh"), 0700)

	err := sshpkg.UpsertHostBlock("myhost", "i-0abc123", "us-west-2", "dev", "ec2-user")
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".ssh", "config"))
	s := string(data)
	if !strings.Contains(s, "Host myhost") {
		t.Errorf("expected Host block, got:\n%s", s)
	}
	if !strings.Contains(s, "i-0abc123") {
		t.Errorf("expected instance ID in config, got:\n%s", s)
	}
}

func TestUpsertHostBlock_updates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sshDir := filepath.Join(dir, ".ssh")
	os.MkdirAll(sshDir, 0700)

	// First write with old instance ID
	sshpkg.UpsertHostBlock("myhost", "i-OLD", "us-west-2", "dev", "ec2-user")
	// Overwrite with new instance ID
	sshpkg.UpsertHostBlock("myhost", "i-NEW", "us-west-2", "dev", "ec2-user")

	data, _ := os.ReadFile(filepath.Join(dir, ".ssh", "config"))
	s := string(data)
	if strings.Contains(s, "i-OLD") {
		t.Errorf("old instance ID should have been replaced:\n%s", s)
	}
	if !strings.Contains(s, "i-NEW") {
		t.Errorf("new instance ID not found:\n%s", s)
	}
}

func TestUpsertHostBlock_preservesExtraLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sshDir := filepath.Join(dir, ".ssh")
	os.MkdirAll(sshDir, 0700)

	sshpkg.UpsertHostBlock("myhost", "i-OLD", "us-west-2", "dev", "ec2-user")

	// Manually add a LocalForward line
	configPath := filepath.Join(dir, ".ssh", "config")
	data, _ := os.ReadFile(configPath)
	amended := strings.Replace(string(data), "Host myhost\n", "Host myhost\n  LocalForward 8080 localhost:8080\n", 1)
	os.WriteFile(configPath, []byte(amended), 0600)

	// Re-upsert with new instance ID
	sshpkg.UpsertHostBlock("myhost", "i-NEW", "us-west-2", "dev", "ec2-user")

	data, _ = os.ReadFile(configPath)
	s := string(data)
	if !strings.Contains(s, "LocalForward 8080 localhost:8080") {
		t.Errorf("LocalForward should have been preserved:\n%s", s)
	}
	if !strings.Contains(s, "i-NEW") {
		t.Errorf("new instance ID not found:\n%s", s)
	}
	if strings.Contains(s, "i-OLD") {
		t.Errorf("old instance ID should have been replaced:\n%s", s)
	}
}
