package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// SelfUpgrade replaces the daemon's own binary (cmd.upgrade). Only `wrangl runner run`
// enables it: tests and development daemons never replace themselves.
type SelfUpgrade struct {
	BinaryPath string                  // the binary systemd starts, e.g. /usr/local/bin/wrangl
	Restart    func(path string) error // default: re-exec path with the same arguments
	Client     *http.Client
}

// MaxBinarySize bounds a downloaded binary.
const MaxBinarySize = 512 << 20

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DefaultSelfUpgrade upgrades the running executable in place.
func DefaultSelfUpgrade() (*SelfUpgrade, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return &SelfUpgrade{BinaryPath: exe}, nil
}

// Sum is the sha256 of the binary on disk.
func (u *SelfUpgrade) Sum() (string, error) {
	f, err := os.Open(u.BinaryPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fetch downloads <controller>/dl/wrangl-linux-<arch>, checks it hashes to want, and
// atomically renames it over BinaryPath. On any failure the old binary is untouched.
func (u *SelfUpgrade) Fetch(ctx context.Context, controller, arch, want string) error {
	if !shaRe.MatchString(want) {
		return fmt.Errorf("bad sha256 %q", want)
	}
	url := strings.TrimRight(controller, "/") + "/dl/wrangl-linux-" + arch
	hc := u.Client
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	dir := filepath.Dir(u.BinaryPath)
	tmp, err := os.CreateTemp(dir, ".wrangl-upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MaxBinarySize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > MaxBinarySize {
		return fmt.Errorf("download %s: larger than %d bytes", url, MaxBinarySize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch: got %s, want %s (binary not replaced)", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), u.BinaryPath)
}

func (u *SelfUpgrade) restart() error {
	if u.Restart != nil {
		return u.Restart(u.BinaryPath)
	}
	// Same pid, same arguments, the new code; systemd sees no exit.
	return syscall.Exec(u.BinaryPath, os.Args, os.Environ())
}
