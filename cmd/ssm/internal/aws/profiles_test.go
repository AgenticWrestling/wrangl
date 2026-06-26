package aws_test

import (
	"os"
	"path/filepath"
	"testing"

	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

func TestListProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte("[default]\n[profile dev]\n[profile staging]\n"), 0600)
	t.Setenv("AWS_CONFIG_FILE", cfg)

	profiles, err := ssmaws.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 3 {
		t.Fatalf("want 3 profiles, got %d: %v", len(profiles), profiles)
	}
}
