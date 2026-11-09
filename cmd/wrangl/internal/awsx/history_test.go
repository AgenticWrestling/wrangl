package awsx_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx"
)

func TestHistoryMRU(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := awsx.SortProfilesByMRU([]string{"a", "b", "c"}); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("no history yet must keep the order: %v", got)
	}
	awsx.RecordProfileUsage("b")
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
