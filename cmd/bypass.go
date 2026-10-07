package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

const dangerouslySkipPermissionsFlag = "--dangerously-skip-permissions"

var bypassCmd = newBypassCommand()

func newBypassCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bypass [on|off]",
		Short: "Toggle Claude Code permission bypass for ccl launches",
		Long: `Toggle permission bypass for every Claude Code session launched by ccl.
When enabled, ccl passes --dangerously-skip-permissions.

Show status:
  ccl bypass

Enable or disable:
  ccl bypass on
  ccl bypass off

Warning: bypass mode allows Claude Code to execute supported operations without
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
		fmt.Fprintln(out, "Warning: Claude Code will start with --dangerously-skip-permissions.")
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

// applyBypassMode adds Claude Code's permission-skip flag once, preserving the
// caller's argument order after the injected flag and never mutating input.
// It stays out of the way when the user chose a permission mode for this run
// (--permission-mode plan would conflict) and when the arguments run a Claude
// Code subcommand (mcp, doctor, ...) rather than a session.
func applyBypassMode(args []string, enabled bool) []string {
	out := append([]string(nil), args...)
	if !enabled {
		return out
	}
	for _, arg := range out {
		if arg == dangerouslySkipPermissionsFlag ||
			arg == "--permission-mode" || strings.HasPrefix(arg, "--permission-mode=") {
			return out
		}
	}
	if len(out) > 0 && isClaudeSubcommand(out[0]) {
		return out
	}
	return append([]string{dangerouslySkipPermissionsFlag}, out...)
}

// claudeSubcommands are Claude Code's own subcommands (claude --help,
// 2.1.287). They are not sessions, so a session flag does not belong on them,
// and a ccl root command must not shadow one by accident.
var claudeSubcommands = []string{
	"agents", "attach", "auth", "auto-mode", "doctor", "gateway", "import", "install",
	"logs", "mcp", "plugin", "project", "respawn", "rm", "setup-token", "stop",
	"ultrareview", "update",
}

func isClaudeSubcommand(arg string) bool {
	for _, name := range claudeSubcommands {
		if arg == name {
			return true
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(bypassCmd)
}
