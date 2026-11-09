package awsx_test

import (
	"os"
	"path/filepath"
	"testing"

	awsx "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx"
)

func TestListProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte("[default]\n[profile dev]\n[profile staging]\n"), 0600)
	t.Setenv("AWS_CONFIG_FILE", cfg)

	profiles, err := awsx.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 3 {
		t.Fatalf("want 3 profiles, got %d: %v", len(profiles), profiles)
	}
}

func TestListProfiles_missingFile(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "nope"))
	p, err := awsx.ListProfiles()
	if err != nil || len(p) != 0 {
		t.Fatalf("got %v %v", p, err)
	}
}
