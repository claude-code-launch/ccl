package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/spf13/cobra"
)

var settingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Preview the settings JSON for the active provider",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := resolveProvider()
		if err != nil {
			return err
		}

		fmt.Println(claude.PreviewSettings(p))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(settingsCmd)
}
