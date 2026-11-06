package agentplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteProducesALoadablePlugin drives Write and then reads what landed on disk, rather
// than asserting over the embedded bytes. The layout is decided in Write (the destination is
// `.claude-plugin`, which go:embed will not carry), so the embedded form and the installed
// form are different shapes and only the second one is what Claude loads.
func TestWriteProducesALoadablePlugin(t *testing.T) {
	dir := t.TempDir()
	changed, err := Write(dir, "1.2.3")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !changed {
		t.Error("first Write reported no change")
	}

	for _, p := range []string{
		".claude-plugin/marketplace.json",
		".claude-plugin/plugin.json",
		"skills/wrangl/SKILL.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// The manifests must be valid JSON after templating. A template that renders a trailing
	// comma or an unquoted version is a file Claude rejects, and nothing else here would catch
	// it: Write only cares that the bytes were written.
	var mp struct {
		Name     string `json:"name"`
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
		Plugins []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &mp); err != nil {
		t.Fatalf("marketplace.json is not valid JSON after templating: %v", err)
	}
	if mp.Name != MarketplaceName {
		t.Errorf("marketplace name = %q, want %q: `claude plugin marketplace update %s` would not find it",
			mp.Name, MarketplaceName, MarketplaceName)
	}
	if len(mp.Plugins) != 1 || mp.Plugins[0].Name != PluginName {
		t.Fatalf("plugins = %+v, want one named %q", mp.Plugins, PluginName)
	}
	// The version must reach BOTH files. It is the only thing that makes an upgrade visible to
	// Claude, and a template that interpolates one and hardcodes the other is the defect this
	// catches.
	if mp.Metadata.Version != "1.2.3" || mp.Plugins[0].Version != "1.2.3" {
		t.Errorf("marketplace.json versions = %q/%q, want 1.2.3 in both",
			mp.Metadata.Version, mp.Plugins[0].Version)
	}
	var pl struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	b, err = os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pl); err != nil {
		t.Fatalf("plugin.json is not valid JSON after templating: %v", err)
	}
	if pl.Version != "1.2.3" {
		t.Errorf("plugin.json version = %q, want 1.2.3", pl.Version)
	}

	// The skill must keep its frontmatter: Claude discovers a skill by `name:` and decides
	// whether to load it from `description:`, so a skill that arrives without them installs
	// cleanly and is never invoked.
	skill, err := os.ReadFile(filepath.Join(dir, "skills", "wrangl", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	head := string(skill)
	if len(head) > 600 {
		head = head[:600]
	}
	if !strings.HasPrefix(head, "---") {
		t.Error("SKILL.md does not start with YAML frontmatter")
	}
	for _, want := range []string{"name: wrangl", "description:"} {
		if !strings.Contains(head, want) {
			t.Errorf("SKILL.md frontmatter is missing %q", want)
		}
	}
}

// TestWriteIsIdempotentOnTheSameVersion is what keeps an unchanged restart from shelling out
// to claude three times. Ensure's cheap path is keyed entirely on this.
func TestWriteIsIdempotentOnTheSameVersion(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, "9.9.9"); err != nil {
		t.Fatal(err)
	}
	changed, err := Write(dir, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second Write at the same version reported a change; Ensure would reload claude on every restart")
	}
	// And a new version must report a change, or an upgrade would never reach Claude. Without
	// this row the test above is satisfied by a Write that always returns false.
	changed, err = Write(dir, "9.9.10")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Write at a new version reported no change; upgrades would be invisible to claude")
	}
}

// TestWriteRefusesAnEmptyVersion pins the polarity. An unversioned plugin is worse than none:
// the marker file would match every later version, so the first install would be the last, and
// every subsequent upgrade would silently keep the old skill.
func TestWriteRefusesAnEmptyVersion(t *testing.T) {
	if _, err := Write(t.TempDir(), ""); err == nil {
		t.Fatal("Write accepted an empty version")
	}
}

// TestTheVersionMarkerIsWrittenLast checks the ordering Write relies on for crash safety: an
// interrupted write must be retried, not recorded as done. Driven by removing the manifest and
// confirming the next Write at the same version puts it back -- which it only can if the
// marker is the last thing written and therefore absent when the write was cut short.
func TestTheVersionMarkerIsWrittenLast(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, "5.0.0"); err != nil {
		t.Fatal(err)
	}
	// Simulate the half-written state by removing the marker, which is what an interruption
	// before the final step leaves behind.
	if err := os.Remove(filepath.Join(dir, versionMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
	changed, err := Write(dir, "5.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Write skipped a directory with no marker: an interrupted write would stay broken")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("plugin.json was not restored: %v", err)
	}
}
