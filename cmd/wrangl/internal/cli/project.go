package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

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
	if f.Changed("limit") {
		vals, _ := f.GetStringSlice("limit")
		r.Limits = map[string]int64{}
		for _, v := range vals {
			name, num, ok := strings.Cut(v, "=")
			n, err := strconv.ParseInt(num, 10, 64)
			if !ok || name == "" || err != nil || n < 0 {
				c.PrintErrln("--limit wants TEMPLATE=N (N >= 0), got", v)
				os.Exit(2)
			}
			r.Limits[name] = n
		}
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
	f.Bool("auto-queue", false, "queue new top-level tasks automatically")
	f.String("notes-repo", "", "linked repo (owner/name) that holds research reports under research/; empty clears")
	f.Bool("citation-audit", false, "research PRs get an audit run that re-fetches every cited URL; unverified citations block auto-merge")
	f.String("agent", "", "default agent definition (wrangl agent list); empty clears")
	f.StringSlice("limit", nil, "max runner instances this project may cause on a template: TEMPLATE=N (repeatable)")
	f.Int64("priority", 0, "scheduling priority across projects (higher first)")
}

func printProject(w io.Writer, p proto.ProjectView) {
	fmt.Fprintf(w, "slug\t%s\nname\t%s\ntemplate\t%s\nplan template\t%s\nauto-merge\t%v\nauto-queue\t%v\npriority\t%d\nnotes repo\t%s\n",
		p.Slug, p.Name, dash(p.Template), dash(p.PlanTemplate), p.AutoMerge, p.AutoQueue, p.Priority, dash(p.NotesRepo))
	if p.CitationAudit {
		fmt.Fprintf(w, "citation audit\ton\n")
	}
	if p.Agent != "" {
		fmt.Fprintf(w, "agent\t%s\n", p.Agent)
	}
	for _, l := range p.Limits {
		note := ""
		if l.Max > l.Ceiling {
			note = fmt.Sprintf(" (effective %d: template max_runners)", l.Ceiling)
		}
		fmt.Fprintf(w, "limit %s\t%d%s\n", l.Template, l.Max, note)
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
			ps, err := c.Projects(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, g, ps, func(w io.Writer) {
				fmt.Fprintln(w, "SLUG\tNAME\tTEMPLATE\tPRIORITY\tAUTO-MERGE\tAUTO-QUEUE")
				for _, p := range ps {
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%v\t%v\n", p.Slug, p.Name, dash(p.Template), p.Priority, p.AutoMerge, p.AutoQueue)
				}
			})
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
	return group("project", "Manage projects", create, list, show, edit, del)
}
