package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/spf13/cobra"
)

func newProviderPreviewCommand(use string) *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Preview the settings JSON injected into Grok Build",
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
	fmt.Println(groklaunch.ConfigOverlay(p))
	return nil
}
