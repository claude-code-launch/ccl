package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/spf13/cobra"
)

var previewCmd = newProviderPreviewCommand("preview [provider]")

func newProviderPreviewCommand(use string) *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Preview the settings JSON injected into Grok Build",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved := *target
			if len(args) == 1 {
				arg := strings.TrimSpace(args[0])
				if resolved.name != "" && arg != "" && resolved.name != arg {
					return fmt.Errorf("specify the provider once, as an argument or with --provider, not both")
				}
				if resolved.name == "" {
					resolved.name = arg
				}
			}
			cmd.SilenceUsage = true
			return runPreview(cmd.OutOrStdout(), resolved)
		},
	}
	addProviderTargetFlags(cmd, target)
	return cmd
}

func runPreview(out io.Writer, target providerTarget) error {
	_, p, err := target.load()
	if err != nil {
		return err
	}
	raw := []byte(groklaunch.ConfigOverlay(p))
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Write(raw)
	}
	fmt.Fprintln(out, buf.String())
	return nil
}

func init() {
	rootCmd.AddCommand(previewCmd)
}
