// Package agentplugin ships the wrangl skill to the Claude Code installation on this host, as
// a plugin marketplace materialised from the binary.
//
// # Why the binary carries it
//
// The skill teaches Claude to drive the `wrangl` CLI, so it is useful exactly where the binary
// is: the controller host and every runner. Neither reliably has a checkout -- a runner is
// built from an AMI and upgrades itself by downloading one file -- so pointing Claude at a
// GitHub marketplace would make the skill depend on network access and on a branch state,
// which a self-upgrading runner has no way to pin. Embedding makes the skill a property of the
// binary's version instead, which is the thing that actually changed.
//
// # Why it is not written into ~/.claude directly
//
// A skill dropped into ~/.claude/skills is a file in somebody else's state directory, with no
// record of who put it there and nothing to uninstall it. The plugin system already answers
// that: `claude plugin marketplace add <path>` registers a source, `install` records the
// decision, and `claude plugin disable|remove wrangl` is a supported way out. So this package
// writes only inside its own directory, and asks `claude` to do the rest.
//
// # Versioning
//
// plugin.json's version IS the binary's version (runner.Version, stamped by ldflags), passed in
// rather than read, because this package must not import the runner. The version is also kept
// in a marker file, which is what makes Ensure cheap on an unchanged restart: no version
// change, no subprocess.
package agentplugin

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// assets holds the skill and the two manifest templates.
//
// The manifests are templates, and stored under assets/ rather than at their final paths,
// because go:embed skips any file or directory whose name begins with a dot unless the pattern
// names it explicitly -- and the destination directory is `.claude-plugin`. Keeping the source
// names neutral and deciding the layout in Write means the embed pattern cannot silently match
// nothing, which is the failure mode that produces an empty plugin and no error.
//
//go:embed assets
var assets embed.FS

// MarketplaceName is the name `claude plugin install` qualifies the plugin with, as
// `wrangl@wrangl`. It is also the name `claude plugin marketplace update` takes.
const MarketplaceName = "wrangl"

// PluginName is the plugin inside that marketplace.
const PluginName = "wrangl"

// versionMarker records the version last written, so an unchanged restart does nothing.
const versionMarker = ".wrangl-version"

// DefaultDir is ~/.wrangl/agent-plugin.
//
// Deliberately not under the controller's data_dir: a runner has no data_dir, the path has to
// be the same on both, and this is the developer's own tooling rather than controller state. It
// is also outside ~/.claude for the reason in the package comment.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".wrangl", "agent-plugin"), nil
}

// Write materialises the plugin into dir and reports whether anything changed.
//
// changed is keyed on the version marker rather than on a file comparison: the skill is one
// file today, and a content diff would make the cheap path depend on reading everything. A
// version that did not move with changed content is a build problem, not something to paper
// over here.
func Write(dir, version string) (changed bool, err error) {
	if version == "" {
		return false, errors.New("version is empty: the plugin's version is the binary's, and an unversioned plugin cannot be upgraded")
	}
	if prev, err := os.ReadFile(filepath.Join(dir, versionMarker)); err == nil && strings.TrimSpace(string(prev)) == version {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		return false, err
	}
	// The skill tree, copied verbatim. Walked rather than listed so a second skill added to
	// assets/skills ships without an edit here.
	skills, err := fs.Sub(assets, "assets/skills")
	if err != nil {
		return false, err
	}
	n := 0
	if err := fs.WalkDir(skills, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "skills", p)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(skills, p)
		if err != nil {
			return err
		}
		n++
		return os.WriteFile(dst, b, 0o644)
	}); err != nil {
		return false, err
	}
	if n == 0 {
		// An embed pattern that matches nothing is not an error to the compiler, so without this
		// the whole package would install an empty plugin and report success.
		return false, errors.New("no skill files were embedded: the go:embed pattern matched nothing")
	}
	for src, dst := range map[string]string{
		"assets/marketplace.json.tmpl": filepath.Join(dir, ".claude-plugin", "marketplace.json"),
		"assets/plugin.json.tmpl":      filepath.Join(dir, ".claude-plugin", "plugin.json"),
	} {
		b, err := fs.ReadFile(assets, src)
		if err != nil {
			return false, err
		}
		t, err := template.New(filepath.Base(src)).Parse(string(b))
		if err != nil {
			return false, fmt.Errorf("%s: %w", src, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, struct{ Version string }{version}); err != nil {
			return false, fmt.Errorf("%s: %w", src, err)
		}
		if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
			return false, err
		}
	}
	// Last, so an interrupted write is retried rather than recorded as done.
	if err := os.WriteFile(filepath.Join(dir, versionMarker), []byte(version+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// claudeBin is the Claude Code CLI, or "" when it is not installed.
func claudeBin() string {
	p, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	return p
}

// run invokes claude with args, returning its combined output.
func run(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Register points Claude at dir and installs the plugin. Safe to repeat: both steps are
// tolerated when they have already happened, because the message differs per Claude version
// and keying on it would break on an upgrade of somebody else's tool.
func Register(ctx context.Context, dir string) error {
	bin := claudeBin()
	if bin == "" {
		return errors.New("claude is not on PATH")
	}
	if out, err := run(ctx, bin, "plugin", "marketplace", "add", dir, "--scope", "user"); err != nil {
		if !strings.Contains(strings.ToLower(out), "already") {
			return fmt.Errorf("marketplace add: %w: %s", err, out)
		}
	}
	if out, err := run(ctx, bin, "plugin", "install", PluginName+"@"+MarketplaceName); err != nil {
		if !strings.Contains(strings.ToLower(out), "already") {
			return fmt.Errorf("plugin install: %w: %s", err, out)
		}
	}
	return nil
}

// Reload makes Claude re-read the marketplace from disk. Writing the files is not enough:
// Claude caches a catalog per marketplace, so a new version on disk is invisible until this
// runs.
func Reload(ctx context.Context) error {
	bin := claudeBin()
	if bin == "" {
		return errors.New("claude is not on PATH")
	}
	if out, err := run(ctx, bin, "plugin", "marketplace", "update", MarketplaceName); err != nil {
		return fmt.Errorf("marketplace update: %w: %s", err, out)
	}
	return nil
}

// Ensure materialises the plugin and, when the version moved, registers and reloads it.
//
// It NEVER returns an error, and that is the design rather than laziness: this runs at the
// start of `controller serve` and of the runner daemon, and a developer's Claude installation
// is not a dependency of either. Every outcome is logged, including the uninteresting one, so
// the absence of a line is not mistaken for success.
func Ensure(ctx context.Context, version string, log *slog.Logger) {
	dir, err := DefaultDir()
	if err != nil {
		log.Warn("agent plugin: cannot locate home directory; skill not installed", "err", err)
		return
	}
	changed, err := Write(dir, version)
	switch {
	case err != nil:
		log.Warn("agent plugin: could not write the skill", "dir", dir, "err", err)
		return
	case !changed:
		log.Debug("agent plugin: already current", "dir", dir, "version", version)
		return
	}
	if claudeBin() == "" {
		// Not a warning: a controller or runner with no Claude Code on it is an ordinary
		// deployment, and the files are on disk for whenever one arrives.
		log.Info("agent plugin: written, but claude is not on PATH so it was not registered",
			"dir", dir, "version", version, "next", "run `wrangl setup` where claude is installed")
		return
	}
	if err := Register(ctx, dir); err != nil {
		log.Warn("agent plugin: written but not registered with claude", "dir", dir, "err", err)
		return
	}
	if err := Reload(ctx); err != nil {
		log.Warn("agent plugin: registered but not reloaded; it updates on the next `claude plugin marketplace update`", "err", err)
		return
	}
	log.Info("agent plugin: skill installed and reloaded", "dir", dir, "version", version,
		"plugin", PluginName+"@"+MarketplaceName)
}
