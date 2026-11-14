package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

var stateGlyph = map[string]string{"open": "○", "in_progress": "▶", "in_review": "◐", "done": "✓", "cancelled": "✗"}

func printTree(w io.Writer, nodes []proto.TaskNode) {
	if len(nodes) == 0 {
		fmt.Fprintln(w, "(no tasks)")
		return
	}
	base := nodes[0].Depth
	for _, n := range nodes {
		extra := ""
		if n.Unmet > 0 {
			extra += fmt.Sprintf("  ⏸ waits on %d", n.Unmet)
		}
		if n.LockedBy != "" {
			extra += "  🔒 " + n.LockedBy
		}
		title := n.Title
		if m := typeMark[n.Type]; m != "" {
			title = m + " " + title
		}
		if n.Total > 0 {
			extra += fmt.Sprintf("  [%d/%d]", n.Done, n.Total)
		}
		if n.Target != "" {
			extra += "  🎯 " + n.Target
			if n.Slip != "" {
				extra += " (" + strings.ReplaceAll(n.Slip, "_", " ") + ")"
			}
		}
		fmt.Fprintf(w, "%s%s %s %s%s\n", strings.Repeat("  ", int(n.Depth-base)), n.Sqid, stateGlyph[n.State], title, extra)
	}
}

// promptArg reads --prompt or -f.
func promptArg(c *cobra.Command) (*string, error) {
	f := c.Flags()
	if f.Changed("file") {
		path, _ := f.GetString("file")
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		s := string(b)
		return &s, nil
	}
	if f.Changed("prompt") {
		s, _ := f.GetString("prompt")
		return &s, nil
	}
	return nil, nil
}

func positionArgs(c *cobra.Command) (first bool, after, before string) {
	f := c.Flags()
	first, _ = f.GetBool("first")
	after, _ = f.GetString("after")
	before, _ = f.GetString("before")
	return
}

const typeHelp = "task type: code, bug, ux, architecture, docs, research, review, release, milestone, feature (default: the parent's, else code)"

// typeMark is the CLI/TUI marker for a task type ("" for code).
var typeMark = map[string]string{"ux": "[ux]", "architecture": "[arch]", "spec": "[docs]", "docs": "[docs]", "bug": "[bug]", "research": "[research]", "review": "[review]", "release": "[RELEASE]", "milestone": "[MILESTONE]", "feature": "[FEATURE]"}

func taskCmd(g *Global) *cobra.Command {
	client := func() (*clientT, error) { return apiClient(g) }

	create := &cobra.Command{
		Use: "create <project>", Short: "Create a task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			f := cmd.Flags()
			req := proto.TaskCreateRequest{Project: args[0]}
			req.Title, _ = f.GetString("title")
			req.Parent, _ = f.GetString("parent")
			req.Repo, _ = f.GetString("repo")
			req.Template, _ = f.GetString("template")
			req.Issue, _ = f.GetString("issue")
			req.Needs, _ = f.GetStringSlice("needs")
			req.Require, _ = f.GetStringSlice("require")
			if v, _ := f.GetStringSlice("need"); len(v) > 0 {
				if req.Need, err = placement.ParsePatch(v); err != nil {
					return err
				}
			}
			req.Type, _ = f.GetString("type")
			req.TargetDate, _ = f.GetString("target-date")
			req.First, req.After, req.Before = positionArgs(cmd)
			if p, err := promptArg(cmd); err != nil {
				return err
			} else if p != nil {
				req.Prompt = *p
			}
			t, err := c.CreateTask(cmd.Context(), req)
			if err != nil {
				return err
			}
			return emit(cmd, g, t, func(w io.Writer) { fmt.Fprintf(w, "created %s: %s\n", t.Sqid, t.Title) })
		},
	}
	cf := create.Flags()
	cf.String("title", "", "title (required)")
	cf.String("prompt", "", "prompt text")
	cf.StringP("file", "f", "", "read the prompt from a markdown file")
	cf.String("parent", "", "parent task sqid (default: top level)")
	cf.String("target-date", "", "target date, YYYY-MM-DD")
	cf.String("repo", "", "owner/name (default: inherited)")
	cf.String("template", "", "runner template (default: inherited)")
	cf.String("issue", "", "linked GitHub issue (owner/name#number)")
	cf.StringSlice("needs", nil, "precursor task sqids")
	cf.StringSlice("require", nil, "capability requirement: arch=arm64, tool:go>=1.22, agent:claude (repeatable)")
	cf.StringSlice("need", nil, "resource need FIELD=VALUE: vcpu=N, ram=8GiB, disk=5GiB (repeatable)")
	cf.String("type", "", typeHelp)
	positionFlags(create)
	create.MarkFlagsMutuallyExclusive("prompt", "file")
	create.MarkFlagRequired("title")

	var treeAll bool
	tree := &cobra.Command{
		Use: "tree <project|sqid>", Short: "Show the ranked task tree", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			nodes, err := c.ProjectTree(cmd.Context(), args[0], treeAll)
			if err != nil { // not a project? try a task sqid
				var terr error
				if nodes, terr = c.TaskTree(cmd.Context(), args[0], treeAll); terr != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			if g.Output != "table" {
				return emit(cmd, g, nodes, nil)
			}
			printTree(out, nodes)
			return nil
		},
	}
	tree.Flags().BoolVar(&treeAll, "all", false, "include done and cancelled tasks")

	var lProject, lState, lParent string
	list := &cobra.Command{
		Use: "list", Short: "List tasks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ts, err := c.Tasks(cmd.Context(), lProject, lState, lParent)
			if err != nil {
				return err
			}
			return emit(cmd, g, ts, func(w io.Writer) {
				fmt.Fprintln(w, "SQID\tSTATE\tTITLE")
				for _, t := range ts {
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.Sqid, t.State, t.Title)
				}
			})
		},
	}
	list.Flags().StringVar(&lProject, "project", "", "only this project")
	list.Flags().StringVar(&lState, "state", "", "only this state")
	list.Flags().StringVar(&lParent, "parent", "", "only children of this sqid")

	show := &cobra.Command{
		Use: "show <sqid>", Short: "Show a task with its precursors, runs and history", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			t, err := c.Task(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, t, func(w io.Writer) {
				fmt.Fprintf(w, "task\t%s %s %s\nproject\t%s\nparent\t%s\ncreated by\t%s\n", t.Sqid, stateGlyph[t.State], t.Title, t.Project, dash(t.Parent), t.CreatedBy)
				if t.Type != "" && t.Type != "code" {
					fmt.Fprintf(w, "type\t%s\n", t.Type)
				}
				if t.EffectiveRepo != "" || t.Repo != "" {
					fmt.Fprintf(w, "repo\t%s\n", repoLine(t.Repo, t.EffectiveRepo))
				}
				if t.TargetDate != "" {
					fmt.Fprintf(w, "target\t%s\n", t.TargetDate)
				}
				printNeed(w, t.Need)
				if len(t.Requires) > 0 {
					fmt.Fprintf(w, "requires\t%s\n", strings.Join(t.Requires, ", "))
				}
				if t.Summary != "" {
					fmt.Fprintf(w, "summary\t%s\n", t.Summary)
				}
				refs := func(label string, rs []proto.TaskRef) {
					for i, r := range rs {
						l := ""
						if i == 0 {
							l = label
						}
						fmt.Fprintf(w, "%s\t%s %s %s\n", l, r.Sqid, stateGlyph[r.State], r.Title)
					}
				}
				refs("subtasks", t.Children)
				refs("needs", t.Precursors)
				refs("needed by", t.Dependents)
				for i, r := range t.Runs {
					l := ""
					if i == 0 {
						l = "runs"
					}
					fmt.Fprintf(w, "%s\t#%d %s %s %s pushed=%v\n", l, r.ID, r.State, dash(r.Runner), r.Branch, r.Pushed)
				}
				for i, e := range t.Events {
					l := ""
					if i == 0 {
						l = "history"
					}
					fmt.Fprintf(w, "%s\t%s %s %s %s\n", l, e.TS, e.Actor, e.Op, string(e.Diff))
				}
				if strings.TrimSpace(t.Prompt) != "" {
					fmt.Fprintf(w, "\n%s\n", t.Prompt)
				}
			})
		},
	}

	edit := &cobra.Command{
		Use: "edit <sqid>", Short: "Edit a task's title, prompt, type or repo", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var req proto.TaskPatchRequest
			if cmd.Flags().Changed("title") {
				t, _ := cmd.Flags().GetString("title")
				req.Title = &t
			}
			if req.Prompt, err = promptArg(cmd); err != nil {
				return err
			}
			if cmd.Flags().Changed("type") {
				t, _ := cmd.Flags().GetString("type")
				req.Type = &t
			}
			if cmd.Flags().Changed("target-date") {
				t, _ := cmd.Flags().GetString("target-date")
				req.TargetDate = &t
			}
			if cmd.Flags().Changed("repo") {
				t, _ := cmd.Flags().GetString("repo")
				req.Repo = &t
			}
			req.Require, _ = cmd.Flags().GetStringSlice("require")
			req.Unrequire, _ = cmd.Flags().GetStringSlice("unrequire")
			if v, _ := cmd.Flags().GetStringSlice("need"); len(v) > 0 {
				if req.Need, err = placement.ParsePatch(v); err != nil {
					return err
				}
			}
			_, err = c.EditTask(cmd.Context(), args[0], req)
			return err
		},
	}
	edit.Flags().StringSlice("require", nil, "capability requirement to add: arch=arm64, tool:go>=1.22 (repeatable)")
	edit.Flags().StringSlice("unrequire", nil, "capability requirement to remove, by key (repeatable)")
	edit.Flags().StringSlice("need", nil, "resource need FIELD=VALUE: vcpu=N, ram=8GiB, disk=5GiB (repeatable; empty value clears)")
	edit.Flags().String("title", "", "new title")
	edit.Flags().String("prompt", "", "new prompt")
	edit.Flags().String("type", "", typeHelp)
	edit.Flags().String("repo", "", "owner/name of a repo linked to the project that this task writes to (empty clears: inherit)")
	edit.Flags().String("target-date", "", "target date, YYYY-MM-DD (empty clears it)")
	edit.Flags().StringP("file", "f", "", "read the new prompt from a file")
	edit.MarkFlagsMutuallyExclusive("prompt", "file")

	move := &cobra.Command{
		Use: "move <sqid>", Short: "Move or rerank a task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var req proto.TaskMoveRequest
			req.Root, _ = cmd.Flags().GetBool("root")
			if cmd.Flags().Changed("parent") {
				p, _ := cmd.Flags().GetString("parent")
				req.Parent = &p
			}
			req.First, req.After, req.Before = positionArgs(cmd)
			_, err = c.MoveTask(cmd.Context(), args[0], req)
			return err
		},
	}
	move.Flags().String("parent", "", "new parent sqid")
	move.Flags().Bool("root", false, "move to top level")
	move.MarkFlagsMutuallyExclusive("parent", "root")
	positionFlags(move)

	needs := group("needs", "Manage precursor tasks",
		&cobra.Command{Use: "add <sqid> <precursor-sqid>", Short: "Add a precursor", Args: cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := client()
				if err != nil {
					return err
				}
				return c.NeedsAdd(cmd.Context(), args[0], args[1])
			}},
		&cobra.Command{Use: "rm <sqid> <precursor-sqid>", Short: "Remove a precursor", Args: cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := client()
				if err != nil {
					return err
				}
				return c.NeedsRm(cmd.Context(), args[0], args[1])
			}},
	)

	split := &cobra.Command{
		Use: "split <sqid>", Short: "Split a task into subtasks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			c, err := client()
			if err != nil {
				return err
			}
			if agent, _ := f.GetBool("agent"); agent {
				guidance, _ := f.GetString("guidance")
				id, err := c.PlanTask(cmd.Context(), args[0], guidance)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "queued plan run %d; its proposal will appear in `wrangl question list`\n", id)
				return nil
			}
			var req proto.TaskSplitRequest
			req.Chain, _ = f.GetBool("chain")
			req.CopyPrompt, _ = f.GetBool("copy-prompt")
			if f.Changed("file") {
				path, _ := f.GetString("file")
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				req.Outline = string(b)
			} else {
				titles, _ := f.GetStringArray("title")
				if len(titles) == 0 {
					return fmt.Errorf("give -f outline.md or one or more --title")
				}
				for _, t := range titles {
					req.Children = append(req.Children, proto.SplitNode{Title: t})
				}
			}
			created, err := c.SplitTask(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			return emit(cmd, g, created, func(w io.Writer) {
				for _, t := range created {
					fmt.Fprintf(w, "created %s: %s\n", t.Sqid, t.Title)
				}
			})
		},
	}
	sf := split.Flags()
	sf.StringP("file", "f", "", "outline file: one subtask per line, indent to nest")
	sf.StringArray("title", nil, "subtask title (repeatable)")
	sf.Bool("chain", false, "each subtask needs the previous one")
	sf.Bool("copy-prompt", false, "copy the parent prompt into each subtask")
	sf.Bool("agent", false, "queue a plan run: an agent proposes the split for you to accept")
	sf.String("guidance", "", "guidance for the planning agent (with --agent)")
	split.MarkFlagsMutuallyExclusive("file", "title", "agent")

	var agentFlag string
	queue := &cobra.Command{
		Use: "queue <sqid>", Short: "Queue a work run for this subtree", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			id, branch, err := c.QueueTaskWith(cmd.Context(), args[0], agentFlag)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "queued run %d (branch %s)\n", id, branch)
			return nil
		},
	}
	queue.Flags().StringVar(&agentFlag, "agent", "", "run with this agent definition instead of the project's default (see `wrangl agent list`)")
	dequeue := &cobra.Command{
		Use: "dequeue <sqid>", Short: "Take a task's queued run back out of the queue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.DequeueTask(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dequeued %s\n", args[0])
			return nil
		},
	}
	ask := &cobra.Command{
		Use: "ask <sqid> <question>", Short: "Ask a controller agent about a task and its subtree", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			agent, _ := cmd.Flags().GetString("agent")
			wait, _ := cmd.Flags().GetBool("wait")
			if err := c.AskTask(cmd.Context(), args[0], agent, args[1]); err != nil {
				return err
			}
			if !wait {
				fmt.Fprintln(cmd.OutOrStdout(), "asked; read the answer with `wrangl task thread "+args[0]+"` or on the task page")
				return nil
			}
			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(2 * time.Second):
				}
				msgs, err := c.TaskThread(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if n := len(msgs); n > 0 && msgs[n-1].Role == "agent" && msgs[n-1].Status != "running" {
					fmt.Fprintln(cmd.OutOrStdout(), msgs[n-1].Body)
					return nil
				}
			}
		},
	}
	ask.Flags().String("agent", "", "controller agent name (default: the default agent)")
	ask.Flags().Bool("wait", false, "wait for and print the answer")
	thread := &cobra.Command{
		Use: "thread <sqid>", Short: "Show a task's conversation with controller agents", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			msgs, err := c.TaskThread(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, m := range msgs {
				body := m.Body
				if m.Status == "running" {
					body = "(thinking…)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s (message %d):\n%s\n", m.At, m.Role, m.ID, body)
				var props []struct{ Label, Action, State, Result string }
				json.Unmarshal([]byte(m.Proposals), &props)
				for i, p := range props {
					st := "approve with: wrangl task apply " + fmt.Sprint(m.ID, " ", i)
					if p.State != "" {
						st = p.State + ": " + p.Result
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  [%d] %s (%s) - %s\n", i, p.Label, p.Action, st)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	apply := &cobra.Command{
		Use: "apply <message-id> <n>", Short: "Approve proposal n (0-based) from `task thread`", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			id, e1 := strconv.ParseInt(args[0], 10, 64)
			n, e2 := strconv.Atoi(args[1])
			if e1 != nil || e2 != nil {
				return fmt.Errorf("message id and n must be numbers")
			}
			if err := c.ApplyProposal(cmd.Context(), id, n); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "applied")
			return nil
		},
	}
	action := func(op, short string) *cobra.Command {
		c := &cobra.Command{Use: op + " <sqid>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := client()
				if err != nil {
					return err
				}
				msg, _ := cmd.Flags().GetString("message")
				return cl.TaskAction(cmd.Context(), args[0], op, msg)
			}}
		if op != "reopen" {
			c.Flags().StringP("message", "m", "", "reason or summary")
		}
		return c
	}
	del := &cobra.Command{
		Use: "delete <sqid>", Short: "Delete a task and its subtree", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return c.DeleteTask(cmd.Context(), args[0])
		},
	}
	return group("task", "Manage the hierarchical work stack",
		create, tree, list, show, edit, move, needs, split, queue, dequeue, ask, thread, apply, taskWaitCmd(g),
		action("complete", "Mark a leaf done (finished and merged)"),
		action("reopen", "Reopen a leaf"),
		action("cancel", "Cancel a task and its unfinished subtasks"),
		del,
	)
}

// repoLine is a task's repo for `task show`: its own, or the one it inherits.
func repoLine(own, effective string) string {
	switch {
	case own != "":
		return own
	case effective != "":
		return effective + " (inherited)"
	}
	return "(none: needs a repo)"
}
