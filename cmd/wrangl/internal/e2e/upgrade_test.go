package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
)

// TestRunnerSelfUpgrade: the controller serves a binary the runner is not on; the runner
// is marked (no new runs), told to upgrade once idle, downloads and verifies it, swaps it
// in and restarts; its next heartbeat clears the mark and queued work runs again.
func TestRunnerSelfUpgrade(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	newBin := []byte("#!/bin/sh\n# wrangl vNext\n")
	sum := sha256.Sum256(newBin)
	want := hex.EncodeToString(sum[:])
	binPath := filepath.Join(t.TempDir(), "wrangl")
	os.WriteFile(binPath, []byte("#!/bin/sh\n# wrangl vPrev\n"), 0o755)

	release := make(chan struct{})
	var mu sync.Mutex
	var restarts []string
	h := newAgentHarness(t, `#!/bin/sh
sqid=$(printf '%s' "$1" | head -1 | sed -n 's/^You are working on task \([A-Za-z0-9]*\):.*/\1/p')
curl -s --unix-socket "$WRANGL_SOCK" -d "{\"sqid\":\"$sqid\",\"summary\":\"ok\"}" http://x/tool/task_complete >/dev/null
`, `name: up
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_sessions: 1
mise_toml: ""
agent:
  start: [%q, "{{.Prompt}}"]
`, nil, harnessOpt{
		beforeRunner: func(data string) {
			os.MkdirAll(filepath.Join(data, "bin"), 0o755)
			os.WriteFile(filepath.Join(data, "bin", "wrangl-linux-"+arch), newBin, 0o755)
		},
		daemon: func(o *runner.DaemonOptions) {
			o.Upgrade = &runner.SelfUpgrade{BinaryPath: binPath, Restart: func(p string) error {
				mu.Lock()
				restarts = append(restarts, p)
				mu.Unlock()
				<-release // hold the "restart" so we can see no run is assigned meanwhile
				return nil
			}}
		},
	})
	defer h.stop()

	runnerView := func() proto.RunnerView {
		rs, _ := h.api.Runners(h.ctx, false)
		if len(rs) != 1 {
			t.Fatalf("runners: %+v", rs)
		}
		return rs[0]
	}
	waitFor(t, "runner told to upgrade and restarting", 10*time.Second, func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		return len(restarts) == 1, fmt.Sprintf("%+v", runnerView())
	})
	if b, _ := os.ReadFile(binPath); string(b) != string(newBin) {
		t.Fatalf("binary not replaced: %q", b)
	}
	if v := runnerView(); v.UpgradeSHA != want {
		t.Fatalf("upgrade not marked pending: %+v", v)
	}

	// While the upgrade is pending, queued work waits.
	task := h.mk(proto.TaskCreateRequest{Title: "after the upgrade"})
	runID, _, err := h.api.QueueTask(h.ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if v := h.run(runID); v.State != "queued" {
		t.Fatalf("run assigned mid-upgrade: %+v", v)
	}
	close(release) // "restarted": the daemon now reports the new binary
	waitFor(t, "upgrade cleared", 10*time.Second, func() (bool, string) {
		v := runnerView()
		return v.UpgradeSHA == "" && v.BinarySHA == want, fmt.Sprintf("%+v", v)
	})
	if v := h.waitRun(runID, "succeeded", "failed"); v.State != "succeeded" {
		t.Fatalf("run after upgrade: %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(restarts) != 1 || restarts[0] != binPath {
		t.Fatalf("restarts: %v", restarts)
	}
}
