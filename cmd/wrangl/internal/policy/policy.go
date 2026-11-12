// Package policy renders and installs the session policy files of ADR 0011 section 6: the host's
// managed settings and a primary worktree's .claude/settings.json, from defaults embedded in the
// binary. The lists are PROPOSED: nothing here runs unless an owner calls `wrangl policy install --apply`.
package policy

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed defaults/*.json
var defaults embed.FS

// Kinds of default file.
const (
	Managed = "managed" // /etc/claude-code/managed-settings.json
	Primary = "primary" // <worktree>/.claude/settings.json
	Run     = "run"     // run agents on the host (6B); applied by the runner, shown but not installed here
)

// DefaultManagedPath is where Claude Code reads managed settings on Linux.
const DefaultManagedPath = "/etc/claude-code/managed-settings.json"

// Vars fill the placeholders in the defaults.
type Vars struct {
	Worktree string // @WORKTREE@: the primary worktree
	Scratch  string // @SCRATCH@: the session scratch directory
	Wrangl   string // @WRANGL@: the wrangl binary the hook runs
}

// Render returns the default file of a kind with its placeholders filled, validated and indented.
func Render(kind string, v Vars) ([]byte, error) {
	raw, err := defaults.ReadFile("defaults/" + kind + ".json")
	if err != nil {
		return nil, fmt.Errorf("unknown policy kind %q (managed, primary, run)", kind)
	}
	s := string(raw)
	for k, val := range map[string]string{"@WORKTREE@": v.Worktree, "@SCRATCH@": v.Scratch, "@WRANGL@": v.Wrangl} {
		if strings.Contains(s, k) && val == "" {
			return nil, fmt.Errorf("the %s policy needs %s", kind, strings.Trim(k, "@"))
		}
		if strings.ContainsAny(val, "\"\\\n") {
			return nil, fmt.Errorf("%s contains a character that cannot go in a settings file: %q", strings.Trim(k, "@"), val)
		}
		s = strings.ReplaceAll(s, k, val)
	}
	var doc any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, fmt.Errorf("policy %s is not valid JSON: %w", kind, err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(doc)
	return out.Bytes(), nil
}

// Target is one file an install would write.
type Target struct {
	Kind    string
	Path    string
	Content []byte
	// Worktree is set for Primary targets (the checkout the file belongs to).
	Worktree string
}

// Options say what to install.
type Options struct {
	ManagedPath string   // "" = DefaultManagedPath; ignored when NoManaged
	NoManaged   bool     // do not touch managed settings
	Worktrees   []string // primary worktrees to write .claude/settings.json into
	ScratchBase string   // scratch dir for a worktree is <ScratchBase>/<worktree name>; "" = <worktree parent>/scratch
	Wrangl      string   // binary path for the hook ("" = wrangl on PATH)
}

// Plan renders every target without touching the disk.
func Plan(o Options) ([]Target, error) {
	wr := o.Wrangl
	if wr == "" {
		wr = "wrangl"
	}
	var out []Target
	if !o.NoManaged {
		p := o.ManagedPath
		if p == "" {
			p = DefaultManagedPath
		}
		b, err := Render(Managed, Vars{Wrangl: wr})
		if err != nil {
			return nil, err
		}
		out = append(out, Target{Kind: Managed, Path: p, Content: b})
	}
	for _, w := range o.Worktrees {
		w = filepath.Clean(w)
		if !filepath.IsAbs(w) {
			return nil, fmt.Errorf("worktree %q must be an absolute path", w)
		}
		scratch := filepath.Join(filepath.Dir(w), "scratch", filepath.Base(w))
		if o.ScratchBase != "" {
			scratch = filepath.Join(o.ScratchBase, filepath.Base(w))
		}
		b, err := Render(Primary, Vars{Worktree: w, Scratch: scratch, Wrangl: wr})
		if err != nil {
			return nil, err
		}
		out = append(out, Target{Kind: Primary, Path: filepath.Join(w, ".claude", "settings.json"), Content: b, Worktree: w})
	}
	if len(out) == 0 {
		return nil, errors.New("nothing to install: give --worktree PATH, or leave managed settings on")
	}
	return out, nil
}

// Status is what an install did (or would do) to one target.
type Status string

const (
	WouldWrite Status = "would write"
	Wrote      Status = "wrote"
	Same       Status = "already up to date"
	Exists     Status = "exists and differs: not overwritten (use --force)"
	Replaced   Status = "replaced (--force)"
	WouldForce Status = "exists and differs: would replace (--force)"
)

// Result is the outcome for one target.
type Result struct {
	Target
	Status Status
}

// Installer applies a plan. Geteuid and the filesystem are explicit so tests never need root.
type Installer struct {
	Apply   bool // false = dry run
	Force   bool // replace a file that exists and differs
	Geteuid func() int
}

// Install dry-runs or applies the plan. It never overwrites an existing file that differs unless
// Force, and writes managed settings only as root (so a session cannot have written them).
func (in Installer) Install(targets []Target) ([]Result, error) {
	geteuid := in.Geteuid
	if geteuid == nil {
		geteuid = os.Geteuid
	}
	var out []Result
	for _, t := range targets {
		r := Result{Target: t}
		existing, err := os.ReadFile(t.Path)
		switch {
		case err == nil && bytes.Equal(existing, t.Content):
			r.Status = Same
		case err == nil && !in.Force:
			r.Status = Exists
		case err == nil:
			r.Status = WouldForce
		case errors.Is(err, os.ErrNotExist):
			r.Status = WouldWrite
		default:
			return out, fmt.Errorf("%s: %w", t.Path, err)
		}
		if in.Apply && (r.Status == WouldWrite || r.Status == WouldForce) {
			if t.Kind == Managed && geteuid() != 0 {
				return out, fmt.Errorf("%s: managed settings are root-owned so a session cannot rewrite them; run with sudo", t.Path)
			}
			if err := write(t); err != nil {
				return out, err
			}
			if r.Status == WouldWrite {
				r.Status = Wrote
			} else {
				r.Status = Replaced
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func write(t Target) error {
	dir := filepath.Dir(t.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := t.Path + ".tmp"
	if err := os.WriteFile(tmp, t.Content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	if t.Kind == Primary {
		// The worktree belongs to the agent user: hand the new file to it when we are root, and keep
		// the file out of `git status` so the host's fast-forward of a clean checkout still works.
		if st, err := os.Stat(t.Worktree); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
				os.Chown(dir, int(sys.Uid), int(sys.Gid))
				os.Chown(t.Path, int(sys.Uid), int(sys.Gid))
			}
		}
		excludeFromGit(t.Worktree, ".claude/settings.json")
	}
	return nil
}

// excludeFromGit adds a path to the checkout's .git/info/exclude (no-op if there is no such
// directory or the line is there).
func excludeFromGit(worktree, rel string) {
	p := filepath.Join(worktree, ".git", "info", "exclude")
	if st, err := os.Stat(filepath.Join(worktree, ".git")); err != nil || !st.IsDir() {
		return
	}
	b, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == rel {
			return
		}
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		f.WriteString("\n")
	}
	f.WriteString(rel + "\n")
}

// Shown is one file `policy show` reports.
type Shown struct {
	Kind, Path string
	Present    bool
	Matches    bool // identical to the embedded default rendered for it
	Content    []byte
}

// Show reads what is in force: the managed file and each worktree's settings file, and compares
// each with the default that install would write.
func Show(o Options) ([]Shown, error) {
	plan, err := Plan(o)
	if err != nil {
		return nil, err
	}
	var out []Shown
	for _, t := range plan {
		s := Shown{Kind: t.Kind, Path: t.Path}
		if b, err := os.ReadFile(t.Path); err == nil {
			s.Present, s.Content = true, b
			s.Matches = bytes.Equal(b, t.Content)
		}
		out = append(out, s)
	}
	return out, nil
}
