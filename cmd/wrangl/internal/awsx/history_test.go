package awsx_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx"
)

func TestHistoryMRUAndLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config", "ssm"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "ssm", "history.json"), []byte(`{"profiles":["b"]}`), 0o600)
	if got := awsx.SortProfilesByMRU([]string{"a", "b", "c"}); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("legacy history ignored: %v", got)
	}
	if err := awsx.RecordProfileUsage("c"); err != nil {
		t.Fatal(err)
	}
	if got := awsx.SortProfilesByMRU([]string{"a", "b", "c"}); !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "wrangl", "dev-history.json")); err != nil {
		t.Fatal(err)
	}
}
