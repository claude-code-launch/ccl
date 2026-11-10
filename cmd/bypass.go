package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var bypassCmd = newBypassCommand()

func newBypassCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bypass [on|off]",
		Short: "Toggle Grok Build permission bypass for ccl launches",
		Long: `Toggle permission bypass for every Grok Build session launched by ccl.
When enabled, ccl passes --always-approve.

Show status:
  ccl bypass

Enable or disable:
  ccl bypass on
  ccl bypass off

Warning: bypass mode allows Grok Build to execute supported operations without
interactive permission prompts. Only enable it in environments you trust.
`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBypass(cmd.OutOrStdout(), args)
		},
	}
}

func runBypass(out io.Writer, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}

	if len(args) == 0 {
		fmt.Fprintf(out, "Bypass = %s\n", onOff(cfg.BypassMode))
		return nil
	}

	enabled, ok := parseBypassOnOff(args[0])
	if !ok {
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if err := config.Update(func(c *provider.Config) error {
		c.BypassMode = enabled
		return nil
	}); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}

	fmt.Fprintf(out, "Bypass = %s\n", onOff(enabled))
	if enabled {
		fmt.Fprintln(out, "Warning: Grok Build will start with --always-approve.")
	}
	return nil
}

func parseBypassOnOff(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on":
		return true, true
	case "off":
		return false, true
	default:
		return false, false
	}
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func init() {
	rootCmd.AddCommand(bypassCmd)
}
