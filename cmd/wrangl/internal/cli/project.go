package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// projectReq builds a request from only the flags the user actually set.
func projectReq(c *cobra.Command) proto.ProjectRequest {
	var r proto.ProjectRequest
	f := c.Flags()
	str := func(name string) *string {
		if !f.Changed(name) {
			return nil
		}
		v, _ := f.GetString(name)
		return &v
	}
	boolean := func(name string) *bool {
		if !f.Changed(name) {
			return nil
		}
		v, _ := f.GetBool(name)
		return &v
	}
	r.Name, r.Description, r.Template, r.PlanTemplate = str("name"), str("description"), str("template"), str("plan-template")
	r.AutoMerge, r.AutoQueue = boolean("auto-merge"), boolean("auto-queue")
	r.NotesRepo, r.Agent = str("notes-repo"), str("agent")
	r.CitationAudit = boolean("citation-audit")
	if f.Changed("allow-template") {
		v, _ := f.GetStringSlice("allow-template")
		r.AllowTemplates = &v
	}
	r.AutoResolve = boolean("auto-resolve")
	r.Require, _ = f.GetStringSlice("require")
	r.Unrequire, _ = f.GetStringSlice("unrequire")
	if f.Changed("need") {
		vals, _ := f.GetStringSlice("need")
		patch, err := placement.ParsePatch(vals)
		if err != nil {
			c.PrintErrln(err)
			os.Exit(2)
		}
		r.Need = patch
	}
	if f.Changed("priority") {
		v, _ := f.GetInt64("priority")
		r.Priority = &v
	}
	return r
}

func projectFlags(c *cobra.Command) {
	f := c.Flags()
	f.String("name", "", "display name")
	f.String("description", "", "description")
	f.String("template", "", "default runner template")
	f.String("plan-template", "", "runner template for plan runs (default: --template)")
	f.Bool("auto-merge", false, "auto-merge PRs that pass every gate and review as low risk")
	f.Bool("auto-resolve", false, "auto-merge queues Smart Merge (an agent merges the base branch in) for a conflicted PR instead of asking you")
	f.Bool("auto-queue", false, "queue new top-level tasks automatically")
	f.String("notes-repo", "", "linked repo (owner/name) that holds research reports under research/; empty clears")
	f.Bool("citation-audit", false, "research PRs get an audit run that re-fetches every cited URL; unverified citations block auto-merge")
	f.String("agent", "", "default agent definition (wrangl agent list); empty clears")
	f.StringSlice("allow-template", nil, "other runner template this project may use, e.g. host (repeatable; replaces the list; empty clears)")
	f.StringSlice("require", nil, "capability requirement for this project's runs: arch=arm64, tool:go>=1.22, agent:claude, net=public|vpn (repeatable)")
	f.StringSlice("unrequire", nil, "remove a capability requirement by key (repeatable)")
	f.StringSlice("need", nil, "default resource need for this project's tasks: vcpu=N, ram=2GiB, disk=5GiB (repeatable; empty value clears, e.g. ram=)")
	f.Int64("priority", 0, "scheduling priority across projects (higher first)")
}

func printProject(w io.Writer, p proto.ProjectView) {
	fmt.Fprintf(w, "slug\t%s\nname\t%s\ntemplate\t%s\nplan template\t%s\nauto-merge\t%v\nauto-queue\t%v\npriority\t%d\nnotes repo\t%s\n",
		p.Slug, p.Name, dash(p.Template), dash(p.PlanTemplate), p.AutoMerge, p.AutoQueue, p.Priority, dash(p.NotesRepo))
	printNeed(w, p.Need)
	if len(p.Requires) > 0 {
		fmt.Fprintf(w, "requires\t%s\n", strings.Join(p.Requires, ", "))
	}
	if len(p.AllowTemplates) > 0 {
		fmt.Fprintf(w, "also allowed\t%s\n", strings.Join(p.AllowTemplates, ", "))
	}
	if p.Archived {
		fmt.Fprintf(w, "archived\t%s\n", p.ArchivedAt)
	}
	if p.AutoResolve {
		fmt.Fprintf(w, "auto-resolve\ton\n")
	}
	if p.CitationAudit {
		fmt.Fprintf(w, "citation audit\ton\n")
	}
	if p.Agent != "" {
		fmt.Fprintf(w, "agent\t%s\n", p.Agent)
	}
	if p.Description != "" {
		fmt.Fprintf(w, "description\t%s\n", p.Description)
	}
}

func projectCmd(g *Global) *cobra.Command {
	create := &cobra.Command{
		Use: "create <slug>", Short: "Create a project", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			req := projectReq(cmd)
			req.Slug = args[0]
			p, err := c.CreateProject(cmd.Context(), req)
			if err != nil {
				return err
			}
			return emit(cmd, g, p, func(w io.Writer) { printProject(w, p) })
		},
	}
	projectFlags(create)
	edit := &cobra.Command{
		Use: "edit <slug>", Short: "Edit a project (only the flags you pass change)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			p, err := c.UpdateProject(cmd.Context(), args[0], projectReq(cmd))
			if err != nil {
				return err
			}
			return emit(cmd, g, p, func(w io.Writer) { printProject(w, p) })
		},
	}
	projectFlags(edit)
	list := &cobra.Command{
		Use: "list", Short: "List projects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			onlyArchived, _ := cmd.Flags().GetBool("archived")
			all, _ := cmd.Flags().GetBool("all")
			if onlyArchived && all {
				return fmt.Errorf("--archived and --all are mutually exclusive")
			}
			filter := map[bool]string{true: "archived", false: "active"}[onlyArchived]
			if all {
				filter = "all"
			}
			ps, err := c.ProjectsFiltered(cmd.Context(), filter)
			if err != nil {
				return err
			}
			return emit(cmd, g, ps, func(w io.Writer) {
				fmt.Fprintln(w, "SLUG\tNAME\tTEMPLATE\tPRIORITY\tAUTO-MERGE\tAUTO-QUEUE\tSTATUS")
				for _, p := range ps {
					status := "active"
					if p.Archived {
						status = "archived"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%v\t%v\t%s\n", p.Slug, p.Name, dash(p.Template), p.Priority, p.AutoMerge, p.AutoQueue, status)
				}
			})
		},
	}
	list.Flags().Bool("archived", false, "show only archived projects")
	list.Flags().Bool("all", false, "show active and archived projects")
	archive := &cobra.Command{
		Use: "archive <slug>", Short: "Archive a project: hide it and its tasks everywhere and stop all work on it (restorable)",
		Long: "Archived projects and their tasks are hidden from lists, the Inbox and search, and nothing runs for them.\n" +
			"Queued runs are cancelled ('project archived'). Archiving is refused while the project has active runs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			res, err := c.ArchiveProject(cmd.Context(), args[0])
			var ref *client.ArchiveRefusedError
			if errors.As(err, &ref) {
				w := cmd.ErrOrStderr()
				fmt.Fprintf(w, "cannot archive %s: it has %d active run(s); let them finish or cancel them first:\n", args[0], len(ref.Runs))
				for _, r := range ref.Runs {
					fmt.Fprintf(w, "  run %d  %s  task %s  %s\n", r.ID, r.State, r.Task, r.Title)
				}
				return fmt.Errorf("project %s has active runs", args[0])
			}
			if err != nil {
				return err
			}
			return emit(cmd, g, res, func(w io.Writer) {
				fmt.Fprintf(w, "archived %s (%d queued run(s) cancelled); restore with: wrangl project restore %s\n", res.Project.Slug, len(res.CancelledRuns), res.Project.Slug)
			})
		},
	}
	restore := &cobra.Command{
		Use: "restore <slug>", Short: "Restore an archived project", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			p, err := c.RestoreProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, p, func(w io.Writer) { fmt.Fprintf(w, "restored %s\n", p.Slug) })
		},
	}
	show := &cobra.Command{
		Use: "show <slug>", Short: "Show a project", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			p, err := c.Project(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, p, func(w io.Writer) { printProject(w, p) })
		},
	}
	del := &cobra.Command{
		Use: "delete <slug>", Short: "Delete a project and all its tasks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.DeleteProject(cmd.Context(), args[0])
		},
	}
	return group("project", "Manage projects", create, list, show, edit, archive, restore, del)
}
