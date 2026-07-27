package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/spf13/cobra"
)

var debugCmd = newDebugCommand()

func newDebugCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "debug [on|off]",
		Short: "Toggle ccl runtime diagnostics for ccl-launched sessions",
		Long: `Toggle runtime diagnostics for every Claude Code session launched by ccl.
When enabled, ccl logs (to /tmp/ccl-debug.log, override with CCL_DEBUG_LOG):

  - runtime startup (provider, backend, protocol, port, credential count)
  - upstream HTTP status / errors (429, 5xx, 401, stream failures)
  - OAuth refresh and cooldown events
  - session metadata (model count, settings size)

It never logs credentials, refresh tokens, or request/response bodies. Use it
to diagnose intermittent "Network error" messages without leaking secrets.

Show status:
  ccl debug

Enable or disable:
  ccl debug on
  ccl debug off
`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebug(cmd.OutOrStdout(), args)
		},
	}
}

func runDebug(out io.Writer, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}

	if len(args) == 0 {
		fmt.Fprintf(out, "Debug = %s\n", onOff(cfg.DebugMode))
		if cfg.DebugMode {
			fmt.Fprintf(out, "Log: %s\n", oauthproxy.ResolveDebugLogPath())
		} else {
			fmt.Fprintln(out, "Enable with: ccl debug on  (logs to /tmp/ccl-debug.log; CCL_DEBUG_LOG overrides)")
		}
		return nil
	}

	enabled, ok := parseDebugOnOff(args[0])
	if !ok {
		return fmt.Errorf("expected on or off, got %q", args[0])
	}
	cfg.DebugMode = enabled
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}

	fmt.Fprintf(out, "Debug = %s\n", onOff(enabled))
	if enabled {
		fmt.Fprintf(out, "Log: %s\n", oauthproxy.ResolveDebugLogPath())
		fmt.Fprintln(out, "Run a ccl session; the log will record runtime startup, upstream errors, OAuth refresh, and session metadata.")
	}
	return nil
}

func parseDebugOnOff(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on":
		applyDebugEnv(true)
		return true, true
	case "off":
		applyDebugEnv(false)
		return false, true
	default:
		return false, false
	}
}

// applyDebugEnv sets the in-process debug sink so subsequent commands in the
// same process see the new state immediately (e.g. `ccl debug on` then a
// doctor/models run within a smoke test). It is harmless when the launcher
// sets it again before Run.
func applyDebugEnv(enabled bool) {
	if !enabled {
		oauthproxy.SetDebug(false, "")
		return
	}
	oauthproxy.SetDebug(true, resolveDebugLogPath())
}

func resolveDebugLogPath() string {
	if v := strings.TrimSpace(os.Getenv("CCL_DEBUG_LOG")); v != "" {
		return v
	}
	return oauthproxy.ResolveDebugLogPath()
}

func init() {
	rootCmd.AddCommand(debugCmd)
}