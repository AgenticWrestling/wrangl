package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type settings struct {
	Permissions struct {
		DefaultMode string   `json:"defaultMode"`
		Allow       []string `json:"allow"`
		Ask         []string `json:"ask"`
		Deny        []string `json:"deny"`
	} `json:"permissions"`
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct{ Type, Command string }
	} `json:"hooks"`
}

func parse(t *testing.T, b []byte) settings {
	t.Helper()
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return s
}

func has(list []string, want ...string) []string {
	var missing []string
	set := map[string]bool{}
	for _, l := range list {
		set[l] = true
	}
	for _, w := range want {
		if !set[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

var vars = Vars{Worktree: "/home/wrangl-agent/work/primary/demo", Scratch: "/home/wrangl-agent/work/primary/scratch/demo", Wrangl: "/usr/local/bin/wrangl"}

// One assertion per row of the ADR 0011 section 6A table.
func TestPrimaryDefaultsCoverTheADRTable(t *testing.T) {
	b, err := Render(Primary, vars)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "@") {
		t.Fatalf("a placeholder is left: %s", b)
	}
	s := parse(t, b)
	if s.Permissions.DefaultMode == "bypassPermissions" {
		t.Error("the primary session must not start with bypassed prompts")
	}
	for name, c := range map[string]struct {
		list []string
		want []string
	}{
		"allow read tools": {s.Permissions.Allow, []string{"Read", "Glob", "Grep", "WebFetch", "WebSearch", "Artifact", "Skill", "Monitor", "ToolSearch", "AskUserQuestion"}},
		"allow edits in the worktree and scratch": {s.Permissions.Allow, []string{
			"Edit(/home/wrangl-agent/work/primary/demo/**)", "Write(/home/wrangl-agent/work/primary/demo/**)",
			"Edit(/home/wrangl-agent/work/primary/scratch/demo/**)", "Write(/home/wrangl-agent/work/primary/scratch/demo/**)"}},
		"allow git": {s.Permissions.Allow, []string{"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git fetch:*)", "Bash(git pull:*)",
			"Bash(git add:*)", "Bash(git commit:*)", "Bash(git checkout:*)", "Bash(git switch:*)", "Bash(git branch:*)", "Bash(git worktree:*)", "Bash(git stash:*)", "Bash(git restore:*)", "Bash(git rebase:*)"}},
		"allow gh":     {s.Permissions.Allow, []string{"Bash(gh pr:*)", "Bash(gh issue:*)", "Bash(gh run:*)", "Bash(gh api:*)"}},
		"allow wrangl": {s.Permissions.Allow, []string{"Bash(wrangl:*)"}},
		"allow dev tools": {s.Permissions.Allow, []string{"Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)", "Bash(go fmt:*)", "Bash(go mod:*)", "Bash(gofmt:*)", "Bash(task:*)", "Bash(mise:*)",
			"Bash(sed:*)", "Bash(grep:*)", "Bash(rg:*)", "Bash(cat:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(sort:*)", "Bash(jq:*)",
			"Bash(diff:*)", "Bash(mkdir:*)", "Bash(sleep:*)", "Bash(timeout:*)", "Bash(python3 -I:*)"}},
		"allow curl (the hook checks the host)": {s.Permissions.Allow, []string{"Bash(curl:*)"}},
		"allow aws reads":                       {s.Permissions.Allow, []string{"Bash(aws ec2 describe-*)", "Bash(aws ce get-*)", "Bash(aws ssm describe-*)", "Bash(aws ssm get-*)"}},
		"allow read-only agents":                {s.Permissions.Allow, []string{"Agent(Explore)", "Agent(Plan)", "Agent(claude-code-guide)"}},
		"ask destructive wrangl": {s.Permissions.Ask, []string{"Bash(wrangl task delete:*)", "Bash(wrangl task cancel:*)", "Bash(wrangl project delete:*)", "Bash(wrangl pr approve:*)",
			"Bash(wrangl pr close:*)", "Bash(wrangl fleet terminate:*)", "Bash(wrangl template delete:*)", "Bash(wrangl secret:*)"}},
		"ask push, merge, restart, ssh": {s.Permissions.Ask, []string{"Bash(git push:*)", "Bash(gh pr merge:*)", "Bash(systemctl --user restart wrangl-controller)", "Bash(ssh:*)"}},
		"deny writing agents":           {s.Permissions.Deny, []string{"Agent(general-purpose)", "Agent(claude)", "Agent(fork)", "Workflow"}},
	} {
		if m := has(c.list, c.want...); len(m) > 0 {
			t.Errorf("%s: missing %v", name, m)
		}
	}
	// Nothing may be both allowed and denied or asked: deny/ask win in Claude Code, so a clash is a typo.
	for _, a := range s.Permissions.Allow {
		for _, d := range append(append([]string{}, s.Permissions.Deny...), s.Permissions.Ask...) {
			if a == d {
				t.Errorf("%q is in allow and in ask/deny", a)
			}
		}
	}
}

func TestManagedDefaultsDenyAndHook(t *testing.T) {
	b, err := Render(Managed, Vars{Wrangl: "/opt/wr/wrangl"})
	if err != nil {
		t.Fatal(err)
	}
	s := parse(t, b)
	want := []string{"Bash(git push --force:*)", "Bash(git reset --hard origin/*)", "Bash(rm -rf:*)", "Bash(sudo:*)", "Bash(chmod -R:*)"}
	for _, p := range []string{"~/.ssh/**", "~/.aws/**", "~/.config/wrangl/**", "~/.local/share/wrangl/**", "~/.claude/settings*.json", "/etc/claude-code/**", "**/*.pem", "**/*.secret", "**/secrets/**", "**/.env*"} {
		for _, tool := range []string{"Read", "Edit", "Write"} {
			want = append(want, tool+"("+p+")")
		}
	}
	if m := has(s.Permissions.Deny, want...); len(m) > 0 {
		t.Errorf("managed deny lacks %v", m)
	}
	pre := s.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "*" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" || pre[0].Hooks[0].Command != "/opt/wr/wrangl hook agent-guard" {
		t.Errorf("hook: %+v", pre)
	}
	if len(s.Permissions.Allow) != 0 {
		t.Error("managed settings should only restrict")
	}
}

func TestRunDefaultsAreNarrow(t *testing.T) {
	b, err := Render(Run, Vars{Worktree: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	s := parse(t, b)
	if m := has(s.Permissions.Deny, "Bash(gh:*)", "Bash(aws:*)", "Bash(cf:*)", "Bash(ssh:*)", "Bash(curl:*)", "Bash(systemctl:*)", "Bash(sudo:*)", "Bash(git push:*)"); len(m) > 0 {
		t.Errorf("run agents may use: %v", m)
	}
	if s.Permissions.DefaultMode != "acceptEdits" || strings.Contains(string(b), "bypass") {
		t.Errorf("mode %q", s.Permissions.DefaultMode)
	}
	if m := has(s.Permissions.Allow, "Edit(/w/**)", "Write(/w/**)", "Bash(python3 -I:*)"); len(m) > 0 {
		t.Errorf("run allow: %v", m)
	}
}

func TestRenderErrors(t *testing.T) {
	if _, err := Render("nope", vars); err == nil {
		t.Error("unknown kind")
	}
	if _, err := Render(Primary, Vars{}); err == nil {
		t.Error("missing placeholders accepted")
	}
	if _, err := Render(Primary, Vars{Worktree: `/a"b`, Scratch: "/s", Wrangl: "w"}); err == nil {
		t.Error("a quote in a path accepted")
	}
}

func TestPlan(t *testing.T) {
	plan, err := Plan(Options{ManagedPath: "/tmp/x/managed.json", Worktrees: []string{"/h/work/primary/demo"}, Wrangl: "/bin/w"})
	if err != nil || len(plan) != 2 {
		t.Fatalf("%v %v", plan, err)
	}
	if plan[0].Kind != Managed || plan[0].Path != "/tmp/x/managed.json" || plan[1].Path != "/h/work/primary/demo/.claude/settings.json" {
		t.Errorf("%+v", plan)
	}
	if !strings.Contains(string(plan[1].Content), "/h/work/primary/scratch/demo/**") {
		t.Error("default scratch dir")
	}
	if p, _ := Plan(Options{NoManaged: true, Worktrees: []string{"/w/demo"}, ScratchBase: "/scr"}); !strings.Contains(string(p[0].Content), "/scr/demo/**") {
		t.Error("scratch base")
	}
	if p, _ := Plan(Options{Worktrees: nil}); p[0].Path != DefaultManagedPath {
		t.Errorf("default managed path: %s", p[0].Path)
	}
	if _, err := Plan(Options{NoManaged: true}); err == nil {
		t.Error("an empty plan")
	}
	if _, err := Plan(Options{NoManaged: true, Worktrees: []string{"relative"}}); err == nil {
		t.Error("relative worktree")
	}
}

func asRoot() int  { return 0 }
func asOwner() int { return 1000 }

func TestInstallDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "etc", "managed-settings.json")
	wt := filepath.Join(dir, "work", "primary", "demo")
	os.MkdirAll(wt, 0o755)
	plan, _ := Plan(Options{ManagedPath: managed, Worktrees: []string{wt}})
	res, err := Installer{Geteuid: asRoot}.Install(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Status != WouldWrite {
			t.Errorf("%s: %s", r.Path, r.Status)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "etc")); err == nil {
		t.Error("a dry run created a directory")
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); err == nil {
		t.Error("a dry run created .claude")
	}
}

func TestInstallApplyThenNeverOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "etc", "claude-code", "managed-settings.json")
	wt := filepath.Join(dir, "work", "primary", "demo")
	os.MkdirAll(filepath.Join(wt, ".git", "info"), 0o755)
	plan, _ := Plan(Options{ManagedPath: managed, Worktrees: []string{wt}, Wrangl: "/bin/w"})
	res, err := Installer{Apply: true, Geteuid: asRoot}.Install(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Status != Wrote {
			t.Errorf("%s: %s", r.Path, r.Status)
		}
		if b, _ := os.ReadFile(r.Path); string(b) != string(r.Content) {
			t.Errorf("%s content", r.Path)
		}
	}
	// The primary file is kept out of git status.
	ex, _ := os.ReadFile(filepath.Join(wt, ".git", "info", "exclude"))
	if !strings.Contains(string(ex), ".claude/settings.json") {
		t.Errorf("exclude: %q", ex)
	}
	// Again: identical files are left alone.
	res, _ = Installer{Apply: true, Geteuid: asRoot}.Install(plan)
	for _, r := range res {
		if r.Status != Same {
			t.Errorf("rerun %s: %s", r.Path, r.Status)
		}
	}
	// The exclude line is not duplicated.
	excludeFromGit(wt, ".claude/settings.json")
	if ex, _ := os.ReadFile(filepath.Join(wt, ".git", "info", "exclude")); strings.Count(string(ex), ".claude/settings.json") != 1 {
		t.Errorf("exclude duplicated: %q", ex)
	}

	// An edited file is never replaced without --force, applied or not.
	os.WriteFile(managed, []byte(`{"owner":"edited"}`), 0o644)
	for _, apply := range []bool{false, true} {
		res, err = Installer{Apply: apply, Geteuid: asRoot}.Install(plan)
		if err != nil || res[0].Status != Exists {
			t.Fatalf("apply=%v: %v %v", apply, res, err)
		}
		if b, _ := os.ReadFile(managed); string(b) != `{"owner":"edited"}` {
			t.Fatalf("apply=%v overwrote the file: %s", apply, b)
		}
	}
	// --force: a dry run says so without writing; --apply --force replaces.
	res, _ = Installer{Force: true, Geteuid: asRoot}.Install(plan)
	if res[0].Status != WouldForce {
		t.Errorf("%s", res[0].Status)
	}
	if b, _ := os.ReadFile(managed); string(b) != `{"owner":"edited"}` {
		t.Fatal("a forced dry run wrote")
	}
	res, _ = Installer{Apply: true, Force: true, Geteuid: asRoot}.Install(plan)
	if res[0].Status != Replaced {
		t.Errorf("%s", res[0].Status)
	}
	if b, _ := os.ReadFile(managed); string(b) != string(plan[0].Content) {
		t.Error("force did not replace")
	}
}

func TestManagedNeedsRoot(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "m.json")
	plan, _ := Plan(Options{ManagedPath: managed})
	_, err := Installer{Apply: true, Geteuid: asOwner}.Install(plan)
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(managed); err == nil {
		t.Error("written without root")
	}
	// A dry run needs no privilege; a worktree file needs none either.
	if _, err := (Installer{Geteuid: asOwner}).Install(plan); err != nil {
		t.Error(err)
	}
	wt := filepath.Join(dir, "wt")
	os.MkdirAll(wt, 0o755)
	p2, _ := Plan(Options{NoManaged: true, Worktrees: []string{wt}})
	if _, err := (Installer{Apply: true, Geteuid: asOwner}).Install(p2); err != nil {
		t.Error(err)
	}
}

func TestShow(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "m.json")
	wt := filepath.Join(dir, "demo")
	os.MkdirAll(wt, 0o755)
	o := Options{ManagedPath: managed, Worktrees: []string{wt}, Wrangl: "w"}
	sh, err := Show(o)
	if err != nil || len(sh) != 2 || sh[0].Present || sh[1].Present {
		t.Fatalf("%+v %v", sh, err)
	}
	plan, _ := Plan(o)
	Installer{Apply: true, Geteuid: asRoot}.Install(plan)
	os.WriteFile(plan[1].Path, []byte(`{"permissions":{}}`), 0o644) // edited
	sh, _ = Show(o)
	if !sh[0].Present || !sh[0].Matches || !sh[1].Present || sh[1].Matches || string(sh[1].Content) != `{"permissions":{}}` {
		t.Errorf("%+v", sh)
	}
}

// globRe turns a Claude Code path pattern (** crosses dirs, * does not) into a regexp.
func globRe(p string) *regexp.Regexp {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	return regexp.MustCompile("^" + b.String() + "$")
}

// Managed settings hit every session on the host, run agents working on wrangl included, so they
// must not deny wrangl's own source files or ordinary branch names.
func TestManagedDoesNotDenyTheReposOwnPaths(t *testing.T) {
	b, err := Render(Managed, Vars{Wrangl: "/opt/wr/wrangl"})
	if err != nil {
		t.Fatal(err)
	}
	s := parse(t, b)
	ok := []string{"cmd/wrangl/internal/api/secrets.go", "cmd/wrangl/internal/controller/secrets.go", "cmd/wrangl/secret.go", "cmd/wrangl/secret_test.go", "docs/secrets.md"}
	for _, path := range ok {
		for _, tool := range []string{"Read", "Edit", "Write"} {
			for _, d := range s.Permissions.Deny {
				if pat, found := strings.CutPrefix(d, tool+"("); found && globRe(strings.TrimSuffix(pat, ")")).MatchString("/work/repo/"+path) {
					t.Errorf("%s %s is denied by %s", tool, path, d)
				}
			}
		}
	}
	// Real credential locations stay denied.
	for _, path := range []string{"/work/repo/deploy/prod.secret", "/work/repo/config/secrets/db.txt"} {
		denied := false
		for _, d := range s.Permissions.Deny {
			if pat, found := strings.CutPrefix(d, "Read("); found && globRe(strings.TrimSuffix(pat, ")")).MatchString(path) {
				denied = true
			}
		}
		if !denied {
			t.Errorf("Read %s should be denied", path)
		}
	}
	for _, d := range s.Permissions.Deny {
		if strings.HasPrefix(d, "Bash(git push") && strings.Contains(d, "main") {
			t.Errorf("%q matches any branch containing main; the agent-guard hook owns the default branch", d)
		}
	}
}
