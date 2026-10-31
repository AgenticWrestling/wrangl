package cli

import "github.com/spf13/cobra"

func questionCmd() *cobra.Command {
	list := leaf("list", "List questions", "M5", cobra.NoArgs)
	list.Flags().Bool("open", true, "only unanswered questions")
	answer := leaf("answer <id> [option-value]", "Answer a question", "M5", cobra.RangeArgs(1, 2))
	answer.Flags().String("text", "", "free-text answer")
	return group("question", "Questions from agents and the controller",
		list,
		leaf("show <id>", "Show a question", "M5", cobra.ExactArgs(1)),
		answer,
	)
}
