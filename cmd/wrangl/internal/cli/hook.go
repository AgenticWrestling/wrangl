package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentguard"
)

func hookCmd() *cobra.Command {
	guard := &cobra.Command{
		Use:   "agent-guard",
		Short: "PreToolUse hook: keep an interactive session from spawning agents outside the work queue",
		Long: "Reads a Claude Code PreToolUse payload (JSON) on stdin and answers on stdout. A denied call gets a\n" +
			"permissionDecision of deny and a reason that names `wrangl delegate`; an allowed call prints nothing, so the\n" +
			"normal permission rules (including ask) still apply. Denies the writing Agent types, run_in_background,\n" +
			"Workflow, a Bash command that starts Claude, gh api calls that are not GET, curl to anything but localhost,\n" +
			"and git push to the default branch or with force. An unreadable payload is allowed (exit 0) with a note on\n" +
			"stderr: a broken hook must not lock the session out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunAgentGuard(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), agentguard.GitEnv())
		},
	}
	return group("hook", "Claude Code hooks shipped in the binary", guard)
}

// RunAgentGuard is the hook's body, with its inputs injected.
func RunAgentGuard(in io.Reader, out, errw io.Writer, env agentguard.Env) error {
	b, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		fmt.Fprintln(errw, "wrangl hook agent-guard:", err)
		return nil
	}
	var payload agentguard.Input
	if err := json.Unmarshal(b, &payload); err != nil {
		fmt.Fprintln(errw, "wrangl hook agent-guard: unreadable payload, allowing:", err)
		return nil
	}
	d := agentguard.Decide(payload, env)
	if !d.Deny {
		return nil
	}
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": d.Reason,
	}})
}
