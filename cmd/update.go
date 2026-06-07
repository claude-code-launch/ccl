package cmd

import (
	"github.com/spf13/cobra"
	"github.com/haiboyuwen/claude-code-launch/internal/claude"
)

var updateCmd = &cobra.Command{
	Use:     "update",
	Aliases: []string{"upgrade"},
	Short:   "Update or install the Claude Code CLI to the latest version",
	Long:    `Checks if Claude Code CLI is installed, and runs the official installer script to update or install it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return claude.Upgrade()
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)
}
