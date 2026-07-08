package cmd

import (
	"github.com/spf13/cobra"
)

var useCmd = &cobra.Command{
	Use:   "use [provider]",
	Short: "Switch the active provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProviderUse(args[0])
	},
}

func init() {
	rootCmd.AddCommand(useCmd)
}
