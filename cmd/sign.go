package cmd

import (
	"fmt"
	"io"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var signCmd = newSignCommand()

func newSignCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sign [on|off]",
		Short: "Hide Claude Code's Co-Authored-By signature in commits and PRs",
		Long: `Hide Claude Code's git attribution for ccl-launched sessions.

Claude Code signs commits and pull requests with a Co-Authored-By trailer and
adds a session link. ccl turns that off by default; ccl sign on restores
Claude Code's own behavior for ccl-launched sessions.

The setting lives in ccl's per-session settings, so a plain ` + "`claude`" + ` is
unchanged. --default also writes it to ~/.claude/settings.json (or
$CLAUDE_CONFIG_DIR/settings.json), which affects every Claude Code session,
including ` + "`ccl use default`" + ` and plain ` + "`claude`" + `:

  ccl sign off --default   # hide the signature everywhere
  ccl sign on --default    # remove the attribution ccl wrote

Turning signing back on only removes the value ccl installed: an attribution
text you wrote yourself is kept.

Show status:
  ccl sign

Configure signing:
  ccl sign on
  ccl sign off
  ccl sign off --default
`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			global, err := cmd.Flags().GetBool("default")
			if err != nil {
				return err
			}
			return runSign(cmd.OutOrStdout(), args, global)
		},
	}
	cmd.Flags().Bool("default", false, "also change Claude Code's global user settings (~/.claude/settings.json)")
	return cmd
}

func runSign(out io.Writer, args []string, global bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	if len(args) == 0 {
		if global {
			return fmt.Errorf("--default needs on or off")
		}
		printSignStatus(out, cfg.SignOn)
		return nil
	}
	enabled, ok := parseBypassOnOff(args[0])
	if !ok {
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	if cfg.SignOn != enabled {
		if err := config.Update(func(c *provider.Config) error {
			c.SignOn = enabled
			return nil
		}); err != nil {
			return fmt.Errorf("save ccl config: %w", err)
		}
	}
	fmt.Fprintf(out, "Signing for ccl sessions = %s\n", onOff(enabled))
	if global {
		changed, err := claude.WriteGlobalSigning(enabled)
		if err != nil {
			return err
		}
		printGlobalSigning(out, changed)
	}
	return nil
}

func printSignStatus(out io.Writer, enabled bool) {
	global := claude.ReadGlobalSigning()
	fmt.Fprintf(out, "Signing for ccl sessions = %s\n", onOff(enabled))
	switch global {
	case claude.GlobalSigningDisabled:
		fmt.Fprintln(out, "Claude Code global settings: attribution hidden")
	case claude.GlobalSigningCustom:
		fmt.Fprintln(out, "Claude Code global settings: custom attribution set")
	default:
		fmt.Fprintln(out, "Claude Code global settings: Claude Code's default signature")
	}
	switch {
	case !enabled && global != claude.GlobalSigningDisabled:
		fmt.Fprintln(out, "Hide it for plain `claude` too with: ccl sign off --default")
	case enabled && global == claude.GlobalSigningDisabled:
		fmt.Fprintln(out, "Plain `claude` still hides it globally; undo with: ccl sign on --default")
	}
}

func printGlobalSigning(out io.Writer, changed bool) {
	path := claude.UserSettingsPath()
	if !changed {
		fmt.Fprintf(out, "%s already matched; nothing changed\n", path)
		return
	}
	fmt.Fprintf(out, "Updated attribution in %s\n", path)
}

func init() {
	rootCmd.AddCommand(signCmd)
}
