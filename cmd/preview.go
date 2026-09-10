package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/spf13/cobra"
)

var previewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview the settings JSON for the active provider",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPreview()
	},
}

func runPreview() error {
	p, err := resolveProvider()
	if err != nil {
		return err
	}

	settings, err := claude.PreviewSettings(p)
	if err != nil {
		return err
	}
	fmt.Println(settings)
	return nil
}

func init() {
	rootCmd.AddCommand(previewCmd)
}
