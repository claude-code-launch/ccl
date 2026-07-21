package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/spf13/cobra"
)

func newFastCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Toggle Claude Code fastMode for the active ChatGPT/Copilot provider",
		Long: `Toggle Claude Code fastMode (the same switch as /fast) for the active
Codex Responses OAuth provider (chatgpt or copilot).

Show status:
  ccl fast

Enable or disable:
  ccl fast on
  ccl fast off

Full form:
  ccl provider fast on
`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFast(cmd.OutOrStdout(), args)
		},
	}
}

var fastCmd = newFastCommand("fast [on|off]")

func runFast(out io.Writer, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	if cfg.ActiveProvider == "" {
		return fmt.Errorf("no active provider; use 'ccl use <name>' first")
	}

	name := cfg.ActiveProvider
	p, ok := cfg.Providers[name]
	if !ok {
		return fmt.Errorf("active provider %q not found; check with 'ccl ls'", name)
	}

	// Show-only mode.
	if len(args) == 0 {
		fmt.Fprintf(out, "%s: Fast = %s\n", name, providerFastSummary(p))
		if !supportsFastMode(p.OAuthProvider) {
			fmt.Fprintf(out, "Note: fastMode is only applied for chatgpt/copilot providers.\n")
		}
		return nil
	}

	on, ok := parseOnOff(args[0])
	if !ok {
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if !supportsFastMode(p.OAuthProvider) {
		return fmt.Errorf("provider %q uses oauthProvider %q; ccl fast only applies to chatgpt and copilot", name, emptyFallback(p.OAuthProvider, "(none)"))
	}

	p.FastMode = on
	cfg.Providers[name] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "%s: Fast = %s\n", name, providerFastSummary(p))
	return nil
}

func parseOnOff(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1", "enable", "enabled":
		return true, true
	case "off", "false", "0", "disable", "disabled":
		return false, true
	default:
		return false, false
	}
}

func emptyFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func init() {
	rootCmd.AddCommand(fastCmd)
}
