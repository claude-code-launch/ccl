package cmd

import "github.com/spf13/cobra"

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run Claude Code with active provider config directly",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runClaude(args)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}
