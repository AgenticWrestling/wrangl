// Package agentguard decides whether a Claude Code tool call may run in a primary session
// (ADR 0011 sections 5 layer 2 and 6): it is the PreToolUse hook behind `wrangl hook agent-guard`.
// It exists to keep an interactive session from starting its own background agents and from
// reaching around the work queue, and to enforce the few rules a permission pattern cannot express
// (HTTP method of gh api, the host of a curl, the branch of a git push).
package agentguard

import (
	"encoding/json"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// Input is the part of the PreToolUse payload the guard reads.
type Input struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
}

// Decision is the guard's verdict. Allow means "no objection": the normal permission rules then
// decide, so an `ask` rule still asks.
type Decision struct {
	Deny   bool
	Reason string
}

// Env supplies what a rule needs from the machine; tests replace it.
type Env struct {
	// DefaultBranch returns the repo's default branch for dir ("" = unknown).
	DefaultBranch func(dir string) string
	// CurrentBranch returns the checked-out branch for dir ("" = unknown).
	CurrentBranch func(dir string) string
}

// GitEnv asks git.
func GitEnv() Env {
	run := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return Env{
		DefaultBranch: func(dir string) string {
			return strings.TrimPrefix(run(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"), "origin/")
		},
		CurrentBranch: func(dir string) string { return run(dir, "rev-parse", "--abbrev-ref", "HEAD") },
	}
}

const delegateHint = " Hand the work to the queue instead: `wrangl delegate \"<title>\" -f prompt.md --queue`, then `wrangl task wait <sqid>`."

func deny(format string, a ...any) Decision {
	return Decision{Deny: true, Reason: sprintf(format, a...)}
}

// readOnlyAgents are the subagent types that cannot write.
var readOnlyAgents = map[string]bool{"Explore": true, "Plan": true, "claude-code-guide": true}

// Decide applies every rule to one tool call.
func Decide(in Input, env Env) Decision {
	switch in.ToolName {
	case "Agent", "Task":
		var ti struct {
			SubagentType    string `json:"subagent_type"`
			RunInBackground bool   `json:"run_in_background"`
		}
		json.Unmarshal(in.ToolInput, &ti)
		typ := ti.SubagentType
		if typ == "" {
			typ = "general-purpose" // the tool's default
		}
		if ti.RunInBackground {
			return deny("A background agent (run_in_background) runs outside the work queue and can starve real runs." + delegateHint)
		}
		if !readOnlyAgents[typ] {
			return deny("The %q agent type can write and run commands on this host outside the work queue. Only Explore, Plan and claude-code-guide are allowed here."+delegateHint, typ)
		}
	case "Workflow":
		return deny("Workflows fan out agents on this host outside the work queue." + delegateHint)
	case "Bash":
		var ti struct {
			Command string `json:"command"`
		}
		json.Unmarshal(in.ToolInput, &ti)
		return decideBash(ti.Command, in.Cwd, env, 0)
	}
	return Decision{}
}

func decideBash(cmd, cwd string, env Env, depth int) Decision {
	for _, argv := range splitCommands(cmd) {
		if d := decideArgv(argv, cwd, env, depth); d.Deny {
			return d
		}
	}
	return Decision{}
}

// wrappers run their arguments as a command.
var wrappers = map[string]bool{"sudo": true, "env": true, "nohup": true, "time": true, "exec": true, "command": true, "nice": true, "setsid": true, "stdbuf": true, "xargs": true, "watch": true}

func decideArgv(argv []string, cwd string, env Env, depth int) Decision {
	// Leading VAR=value assignments and wrapper commands do not change what runs.
	for len(argv) > 0 {
		switch {
		case isAssign(argv[0]):
			argv = argv[1:]
		case wrappers[filepath.Base(argv[0])]:
			argv = argv[1:]
			for len(argv) > 0 && (strings.HasPrefix(argv[0], "-") || isAssign(argv[0])) {
				argv = argv[1:]
			}
		case filepath.Base(argv[0]) == "timeout" && len(argv) > 1:
			argv = argv[1:]
			for len(argv) > 0 && (strings.HasPrefix(argv[0], "-") || isDuration(argv[0])) {
				argv = argv[1:]
			}
		default:
			goto run
		}
	}
run:
	if len(argv) == 0 {
		return Decision{}
	}
	name := filepath.Base(argv[0])
	args := argv[1:]
	switch name {
	case "bash", "sh", "zsh", "dash":
		for i, a := range args { // sh -c "<command>": look inside
			if a == "-c" && i+1 < len(args) && depth < 3 {
				return decideBash(args[i+1], cwd, env, depth+1)
			}
		}
	case "claude":
		if len(args) == 1 && (args[0] == "--version" || args[0] == "-v" || args[0] == "--help" || args[0] == "-h") {
			return Decision{}
		}
		return deny("Starting another Claude (`claude ...`) from here runs an agent on this host outside the work queue." + delegateHint)
	case "tmux", "zellij", "screen":
		for _, a := range args {
			if strings.Contains(a, "claude") {
				return deny("Running Claude inside %s starts an agent on this host outside the work queue."+delegateHint, name)
			}
		}
	case "gh":
		return decideGH(args)
	case "curl":
		return decideCurl(args)
	case "git":
		return decideGit(args, cwd, env)
	}
	return Decision{}
}

func isAssign(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for _, c := range s[:i] {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isDuration(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == '.' || c == 's' || c == 'm' || c == 'h' || c == 'd') {
			return false
		}
	}
	return true
}

// decideGH rejects `gh api` calls that are not GET: -X/--method other than GET, and the implicit
// POST gh makes when body fields are given (also every graphql call, which is a POST).
func decideGH(args []string) Decision {
	if len(args) == 0 || args[0] != "api" {
		return Decision{}
	}
	method := ""
	body := false
	endpoint := ""
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--method="):
			method = strings.TrimPrefix(a, "--method=")
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = a[2:]
		case a == "-f" || a == "-F" || a == "--field" || a == "--raw-field" || a == "--input":
			body = true
			i++
		case strings.HasPrefix(a, "--field=") || strings.HasPrefix(a, "--raw-field=") || strings.HasPrefix(a, "--input="):
			body = true
		case a == "-H" || a == "--header" || a == "-q" || a == "--jq" || a == "-t" || a == "--template" || a == "--cache" || a == "--hostname" || a == "-p" || a == "--preview":
			i++
		case !strings.HasPrefix(a, "-") && endpoint == "":
			endpoint = a
		}
	}
	switch {
	case method != "" && !strings.EqualFold(method, "GET"):
		return deny("`gh api` with method %s changes GitHub. Only GET is allowed here; use `gh pr`/`gh issue` for the supported actions or ask the owner.", strings.ToUpper(method))
	case method == "" && (body || endpoint == "graphql"):
		return deny("`gh api` with body fields (or graphql) is a POST and changes GitHub. Only GET is allowed here; add `-X GET` for a read, or ask the owner.")
	}
	return Decision{}
}

// curl options that take a separate value, so the value is not mistaken for a URL.
var curlValueOpts = map[string]bool{
	"-H": true, "--header": true, "-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
	"-o": true, "--output": true, "-X": true, "--request": true, "-u": true, "--user": true, "-A": true, "--user-agent": true,
	"-e": true, "--referer": true, "-b": true, "--cookie": true, "-c": true, "--cookie-jar": true, "-F": true, "--form": true,
	"-m": true, "--max-time": true, "--connect-timeout": true, "-w": true, "--write-out": true, "-T": true, "--upload-file": true,
	"--retry": true, "--cacert": true, "--cert": true, "--key": true, "-E": true, "-r": true, "--range": true, "-z": true,
}

func decideCurl(args []string) Decision {
	var urls []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-K" || a == "--config" || strings.HasPrefix(a, "--config=") || a == "-x" || a == "--proxy" || strings.HasPrefix(a, "--proxy=") ||
			a == "--resolve" || a == "--connect-to":
			return deny("`curl` with %s can reach hosts this check cannot see. Only curl to localhost / 127.0.0.1 is allowed here.", a)
		case a == "--url" && i+1 < len(args):
			urls = append(urls, args[i+1])
			i++
		case strings.HasPrefix(a, "--url="):
			urls = append(urls, strings.TrimPrefix(a, "--url="))
		case curlValueOpts[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			urls = append(urls, a)
		}
	}
	for _, u := range urls {
		if !localURL(u) {
			return deny("`curl %s` reaches outside this machine. Only curl to localhost / 127.0.0.1 is allowed here; use WebFetch to read a public page.", u)
		}
	}
	return Decision{}
}

func localURL(raw string) bool {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "::1" || h == "127.0.0.1" || strings.HasPrefix(h, "127.")
}

// decideGit rejects pushes that force, push every branch, or land on the default branch.
func decideGit(args []string, cwd string, env Env) Decision {
	// Skip git's global options to find the subcommand.
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "-C" || args[i] == "-c" || args[i] == "--git-dir" || args[i] == "--work-tree" {
			if args[i] == "-C" && i+1 < len(args) {
				if filepath.IsAbs(args[i+1]) {
					cwd = args[i+1]
				} else {
					cwd = filepath.Join(cwd, args[i+1])
				}
			}
			i++
		}
		i++
	}
	if i >= len(args) || args[i] != "push" {
		return Decision{}
	}
	var rest []string
	for _, a := range args[i+1:] {
		switch {
		case a == "-f" || a == "--force" || strings.HasPrefix(a, "--force-with-lease") || a == "--force-if-includes":
			return deny("`git push --force` rewrites shared history and is not allowed here.")
		case a == "--all" || a == "--mirror":
			return deny("`git push %s` would push the default branch too. Push the one branch you mean.", a)
		case strings.HasPrefix(a, "-"):
		default:
			rest = append(rest, a)
		}
	}
	def := ""
	cur := ""
	if env.DefaultBranch != nil {
		def = env.DefaultBranch(cwd)
	}
	if env.CurrentBranch != nil {
		cur = env.CurrentBranch(cwd)
	}
	isDefault := func(b string) bool {
		b = strings.TrimPrefix(strings.TrimPrefix(b, "refs/heads/"), "+")
		return b == "main" || b == "master" || (def != "" && b == def)
	}
	msg := "`git push` to the default branch is not allowed here. Push a feature branch and open a PR, or " + strings.TrimSpace(delegateHint)
	if len(rest) <= 1 { // no refspec: the current branch goes to its upstream
		if isDefault(cur) {
			return deny("%s", msg)
		}
		return Decision{}
	}
	for _, ref := range rest[1:] {
		if strings.HasPrefix(ref, "+") {
			return deny("`git push +%s` is a forced push and is not allowed here.", strings.TrimPrefix(ref, "+"))
		}
		dst := ref
		if j := strings.IndexByte(ref, ':'); j >= 0 {
			dst = ref[j+1:]
			if ref[:j] == "" { // ":branch" deletes it
				return deny("Deleting a remote branch with `git push` is not allowed here.")
			}
		} else if ref == "HEAD" {
			dst = cur
		}
		if dst == "HEAD" {
			dst = cur
		}
		if isDefault(dst) {
			return deny("%s", msg)
		}
	}
	return Decision{}
}
