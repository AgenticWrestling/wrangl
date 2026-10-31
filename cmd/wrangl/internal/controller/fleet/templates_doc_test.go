package fleet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// TestDocTemplatesParse keeps the example templates in docs/wrangl/templates valid.
func TestDocTemplatesParse(t *testing.T) {
	files, _ := filepath.Glob("../../../../../docs/wrangl/templates/*.yaml")
	if len(files) < 3 {
		t.Fatalf("example templates not found: %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p, err := ParseTemplate(b)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		spec, err := proto.ParseAgentSpec(p.AgentJson)
		if err != nil || len(spec.Start) == 0 {
			t.Errorf("%s: agent block: %v %+v", filepath.Base(f), err, spec)
		}
		if filepath.Base(f) == "claude-interactive.yaml" && (spec.Drive != proto.DriveInteractive || len(spec.Expect) == 0 || len(spec.SecretFiles) != 1) {
			t.Errorf("claude-interactive: %+v", spec)
		}
	}
}
