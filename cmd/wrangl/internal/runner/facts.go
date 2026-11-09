package runner

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// unameArch maps GOARCH to `uname -m` naming.
func unameArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return runtime.GOARCH
}

// MeasureFacts reports this machine's arch, vCPUs, RAM and root disk size.
func MeasureFacts(ctx context.Context) (proto.Facts, error) {
	f := proto.Facts{Arch: unameArch()}
	n, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		return f, err
	}
	f.VCPU = n
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return f, err
	}
	f.RAMMiB = int(vm.Total >> 20)
	du, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		return f, err
	}
	f.DiskGiB = int(du.Total >> 30)
	f.Tags = ProbeTags()
	return f, nil
}

// probedTools are looked for on PATH; each found one becomes a tool:NAME tag.
var probedTools = []string{"git", "gh", "go", "node", "python3", "cargo", "docker", "mise"}

// ProbeTags reports what this machine can vouch for about itself: tools on PATH, and agent CLIs it is
// signed in to (claude, when its OAuth token or credentials file is present).
func ProbeTags() []string {
	var tags []string
	for _, t := range probedTools {
		if _, err := exec.LookPath(t); err == nil {
			tags = append(tags, "tool:"+t)
		}
	}
	if _, err := exec.LookPath("claude"); err == nil {
		home, _ := os.UserHomeDir()
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" || fileExists(filepath.Join(home, ".claude", ".credentials.json")) {
			tags = append(tags, "agent:claude=authed")
		}
	}
	return tags
}

func fileExists(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }

// SampleHeartbeat collects current utilisation.
func SampleHeartbeat(ctx context.Context, version string) proto.Heartbeat {
	hb := proto.Heartbeat{Time: time.Now().UTC(), Version: version}
	if p, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(p) == 1 {
		hb.CPUPercent = p[0]
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		hb.MemPercent = vm.UsedPercent
	}
	if du, err := disk.UsageWithContext(ctx, "/"); err == nil {
		hb.DiskPercent = du.UsedPercent
	}
	if l, err := load.AvgWithContext(ctx); err == nil {
		hb.Load1 = l.Load1
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		hb.UptimeS = up
	}
	return hb
}

// InstanceID asks IMDSv2 for the EC2 instance id. Off EC2 (or with IMDS blocked) it
// returns "" so an adopted box can still enrol with a token alone.
func InstanceID(ctx context.Context, imdsURL string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}} // IMDS is link-local: never via a proxy
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, imdsURL+"/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, imdsURL+"/latest/meta-data/instance-id", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	id, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(id), "i-") {
		return ""
	}
	return strings.TrimSpace(string(id))
}
