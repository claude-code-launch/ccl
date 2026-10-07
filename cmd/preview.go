package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/spf13/cobra"
)

var previewCmd = newProviderPreviewCommand("preview")

func newProviderPreviewCommand(use string) *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Preview the settings JSON injected into Claude Code",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreview(*target)
		},
	}
	addProviderTargetFlags(cmd, target)
	return cmd
}

func runPreview(target providerTarget) error {
	_, p, err := target.load()
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
	rootCmd.AddCommand(deprecatedRootAlias(previewCmd, "ccl provider preview"))
}
