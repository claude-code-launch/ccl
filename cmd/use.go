package cmd

import (
	"github.com/spf13/cobra"
)

var useCmd = &cobra.Command{
	Use:   "use [provider]",
	Short: "Switch the active provider",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		return runProviderUse(name)
	},
}

func init() {
	rootCmd.AddCommand(useCmd)
}
