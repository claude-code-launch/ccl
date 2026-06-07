package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/claude-code-launch/internal/claude"
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