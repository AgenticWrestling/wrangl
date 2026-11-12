package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// DelegatedTitle is the per-project feature that delegated tasks are created under (ADR 0011
// section 5, layer 1).
const DelegatedTitle = "Delegated"

// taskAPI is what delegate and wait need from the controller client.
type taskAPI interface {
	Tasks(ctx context.Context, project, state, parent string) ([]proto.TaskRef, error)
	Task(ctx context.Context, sqid string) (proto.TaskDetail, error)
	CreateTask(ctx context.Context, req proto.TaskCreateRequest) (proto.TaskRef, error)
	QueueTask(ctx context.Context, sqid string) (int64, string, error)
}

// delegatedFeature returns the project's Delegated feature, creating it on first use.
func delegatedFeature(ctx context.Context, c taskAPI, project string) (string, error) {
	refs, err := c.Tasks(ctx, project, "", "")
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Title != DelegatedTitle || r.State == "cancelled" {
			continue
		}
		d, err := c.Task(ctx, r.Sqid)
		if err != nil {
			return "", err
		}
		if d.Type == "feature" && d.Parent == "" {
			return d.Sqid, nil
		}
	}
	f, err := c.CreateTask(ctx, proto.TaskCreateRequest{Project: project, Title: DelegatedTitle, Type: "feature",
		Prompt: "Work handed off from interactive sessions with `wrangl delegate`."})
	if err != nil {
		return "", fmt.Errorf("create the %s feature: %w", DelegatedTitle, err)
	}
	return f.Sqid, nil
}

// Delegate creates a task under the project's Delegated feature and optionally queues it.
func Delegate(ctx context.Context, c taskAPI, project, title, prompt, typ string, queue bool) (task proto.TaskRef, runID int64, err error) {
	feat, err := delegatedFeature(ctx, c, project)
	if err != nil {
		return task, 0, err
	}
	if typ == "" {
		typ = "code" // do not inherit the feature's type
	}
	task, err = c.CreateTask(ctx, proto.TaskCreateRequest{Project: project, Parent: feat, Title: title, Prompt: prompt, Type: typ})
	if err != nil {
		return task, 0, err
	}
	if queue {
		runID, _, err = c.QueueTask(ctx, task.Sqid)
		if err != nil {
			return task, 0, fmt.Errorf("created %s but could not queue it: %w", task.Sqid, err)
		}
	}
	return task, runID, nil
}

// inferProject picks the project when --project is not given: WRANGL_PROJECT, else the project of
// the task this agent is working on (WRANGL_TASK), else the <slug> of a ".../primary/<slug>" cwd.
func inferProject(ctx context.Context, c taskAPI) (string, error) {
	if p := os.Getenv("WRANGL_PROJECT"); p != "" {
		return p, nil
	}
	if sq := os.Getenv("WRANGL_TASK"); sq != "" {
		if d, err := c.Task(ctx, sq); err == nil {
			return d.Project, nil
		}
	}
	if wd, err := os.Getwd(); err == nil {
		parts := strings.Split(filepath.ToSlash(wd), "/")
		for i := len(parts) - 2; i >= 0; i-- {
			if parts[i] == "primary" && parts[i+1] != "" {
				return parts[i+1], nil
			}
		}
	}
	return "", errors.New("which project? pass --project (or set WRANGL_PROJECT)")
}

func delegateCmd(g *Global) *cobra.Command {
	var project, typ string
	var queue bool
	c := &cobra.Command{
		Use:   `delegate "<title>" [-f prompt.md | --prompt TEXT] [--queue] [--project P]`,
		Short: "Hand work to a runner: create a task under the project's Delegated feature and optionally queue it",
		Long: "Creates a task under the project's \"Delegated\" feature (made on first use) and, with --queue, queues it\n" +
			"for a runner in the same step. Follow it with `wrangl task wait SQID`. The project defaults to\n" +
			"WRANGL_PROJECT, the project of WRANGL_TASK, or the <slug> of a .../primary/<slug> directory.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := apiClient(g)
			if err != nil {
				return err
			}
			if project == "" {
				if project, err = inferProject(cmd.Context(), cl); err != nil {
					return err
				}
			}
			prompt := ""
			if p, err := promptArg(cmd); err != nil {
				return err
			} else if p != nil {
				prompt = *p
			}
			t, run, err := Delegate(cmd.Context(), cl, project, args[0], prompt, typ, queue)
			if err != nil {
				return err
			}
			out := map[string]any{"sqid": t.Sqid, "title": t.Title, "project": project, "queued": queue, "run": run}
			return emit(cmd, g, out, func(w io.Writer) {
				fmt.Fprintf(w, "delegated %s: %s\n", t.Sqid, t.Title)
				if queue {
					fmt.Fprintf(w, "queued (run %d); wait for it with: wrangl task wait %s\n", run, t.Sqid)
				} else {
					fmt.Fprintf(w, "not queued; queue it with: wrangl task queue %s\n", t.Sqid)
				}
			})
		},
	}
	f := c.Flags()
	f.String("prompt", "", "prompt text")
	f.StringP("file", "f", "", "read the prompt from a markdown file")
	f.StringVar(&project, "project", "", "project slug (default: inferred, see above)")
	f.StringVar(&typ, "type", "", typeHelp)
	f.BoolVar(&queue, "queue", false, "queue the task for a runner now")
	c.MarkFlagsMutuallyExclusive("prompt", "file")
	return c
}

// WaitResult is how a wait ended.
type WaitResult struct {
	Outcome string // done | review | cancelled | failed | timeout
	Task    proto.TaskDetail
}

// Failed is true for outcomes a script should treat as not-success.
func (r WaitResult) Failed() bool { return r.Outcome != "done" && r.Outcome != "review" }

// classify says whether a task is finished and how. A task is finished when it is done, waiting for
// the owner's review (the agent completed it), cancelled, or open again after its last run ended
// without success.
func classify(t proto.TaskDetail) (string, bool) {
	switch t.State {
	case "done":
		return "done", true
	case "in_review":
		return "review", true
	case "cancelled":
		return "cancelled", true
	case "open":
		var last *proto.TaskRunView
		for i := range t.Runs {
			if last == nil || t.Runs[i].ID > last.ID {
				last = &t.Runs[i]
			}
		}
		if last != nil && proto.Terminal(last.State) && last.State != "succeeded" {
			return "failed", true
		}
	}
	return "", false
}

// WaitForTask polls the task until it is finished or timeout passes (0 = no timeout).
func WaitForTask(ctx context.Context, fetch func(context.Context, string) (proto.TaskDetail, error), sqid string, timeout, every time.Duration) (WaitResult, error) {
	if every <= 0 {
		every = 5 * time.Second
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	var last proto.TaskDetail
	for {
		t, err := fetch(ctx, sqid)
		if err != nil {
			return WaitResult{Task: last}, err
		}
		last = t
		if o, done := classify(t); done {
			return WaitResult{Outcome: o, Task: t}, nil
		}
		select {
		case <-ctx.Done():
			return WaitResult{Task: last}, ctx.Err()
		case <-deadline:
			return WaitResult{Outcome: "timeout", Task: last}, nil
		case <-time.After(every):
		}
	}
}

func printOutcome(w io.Writer, r WaitResult) {
	t := r.Task
	fmt.Fprintf(w, "%s %s: %s\n", t.Sqid, t.Title, map[string]string{
		"done": "done", "review": "finished, waiting for your review", "cancelled": "cancelled",
		"failed": "failed (its last run ended without success; it is open again)", "timeout": "still running (timed out waiting)",
	}[r.Outcome])
	fmt.Fprintf(w, "state\t%s\n", t.State)
	if t.Summary != "" {
		fmt.Fprintf(w, "summary\t%s\n", t.Summary)
	}
	for _, run := range t.Runs {
		fmt.Fprintf(w, "run %d\t%s%s\n", run.ID, run.State, branchNote(run))
	}
}

func branchNote(r proto.TaskRunView) string {
	if r.Branch == "" {
		return ""
	}
	n := " · " + r.Branch
	if r.Pushed {
		n += " (pushed)"
	}
	return n
}

func taskWaitCmd(g *Global) *cobra.Command {
	var timeout, every time.Duration
	c := &cobra.Command{
		Use:   "wait <sqid>",
		Short: "Block until a task is finished and print the outcome",
		Long: "Polls the task until it is done, waiting for your review (the agent completed it), cancelled, or open\n" +
			"again after a failed run, then prints the state, summary and runs. Exit status 0 for done or review,\n" +
			"1 for cancelled, failed or timeout.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := apiClient(g)
			if err != nil {
				return err
			}
			r, err := WaitForTask(cmd.Context(), cl.Task, args[0], timeout, every)
			if err != nil {
				return err
			}
			if err := emit(cmd, g, map[string]any{"outcome": r.Outcome, "task": r.Task}, func(w io.Writer) { printOutcome(w, r) }); err != nil {
				return err
			}
			if r.Failed() {
				cmd.SilenceUsage = true
				return fmt.Errorf("task %s: %s", args[0], r.Outcome)
			}
			return nil
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up after this long (0 = wait forever)")
	c.Flags().DurationVar(&every, "interval", 5*time.Second, "how often to look")
	return c
}
