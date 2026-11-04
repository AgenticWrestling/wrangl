package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func printQuestion(w io.Writer, q proto.QuestionView) {
	fmt.Fprintf(w, "question\t#%d (%s, %s)\ntask\t%s %s\nproject\t%s\n", q.ID, q.Kind, q.State, q.Task, q.TaskTitle, q.Project)
	if q.RunID != 0 {
		fmt.Fprintf(w, "run\t%d\n", q.RunID)
	}
	fmt.Fprintf(w, "asks\t%s\n", q.Prompt)
	for i, o := range q.Options {
		l := ""
		if i == 0 {
			l = "options"
		}
		d := ""
		if o.Description != "" {
			d = " — " + o.Description
		}
		fmt.Fprintf(w, "%s\t%s: %s%s\n", l, o.Value, o.Label, d)
	}
	if q.AllowFreeText {
		fmt.Fprintf(w, "\tor answer with --text\n")
	}
	if q.Answer != "" {
		fmt.Fprintf(w, "answer\t%s\n", q.Answer)
	}
	if q.DetailMD != "" {
		fmt.Fprintf(w, "\n%s\n", q.DetailMD)
	}
}

func questionCmd(g *Global) *cobra.Command {
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List open questions (from agents, approvals, PR and task reviews)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			qs, err := c.Questions(cmd.Context(), all)
			if err != nil {
				return err
			}
			return emit(cmd, g, qs, func(w io.Writer) {
				fmt.Fprintln(w, "ID\tKIND\tSTATE\tTASK\tRUN\tQUESTION")
				for _, q := range qs {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\n", q.ID, q.Kind, q.State, q.Task, q.RunID, q.Prompt)
				}
			})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include answered and cancelled questions")
	show := &cobra.Command{
		Use: "show <id>", Short: "Show a question and its options", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("question id %q: want a number", args[0])
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			q, err := c.Question(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emit(cmd, g, q, func(w io.Writer) { printQuestion(w, q) })
		},
	}
	var text, option string
	answer := &cobra.Command{
		Use: "answer <id> [option-value]", Short: "Answer a question with an option and/or --text", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("question id %q: want a number", args[0])
			}
			value := ""
			if len(args) == 2 {
				value = args[1]
			}
			if option != "" {
				if value != "" && value != option {
					return fmt.Errorf("give the option once: %q or --option %q", value, option)
				}
				value = option
			}
			if value == "" && text == "" {
				return fmt.Errorf("give an option value or --text")
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.Answer(cmd.Context(), id, value, text); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "answered question %d\n", id)
			return nil
		},
	}
	answer.Flags().StringVar(&option, "option", "", "option value to choose (same as the positional argument), e.g. accept|reopen for a task_review")
	answer.Flags().StringVar(&text, "text", "", "free-text answer (or extra detail with an option)")
	return group("question", "Questions from agents, approvals and PR reviews", list, show, answer)
}
