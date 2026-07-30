package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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

Each session writes its own log next to the configured path, named after the
settings file ccl generates for it, e.g. ~/.ccl/logs/ccl-debug-claude_9f2c1b7d4e05.log
(base path overridable with CCL_DEBUG_LOG). The exact path is printed when the
session ends.

A session log records:

  - session start/exit, and failures that stop a session from launching
  - runtime startup and teardown (provider, backend, protocol, port, credentials)
  - upstream HTTP status / errors (400 context limits, 401, 429, 5xx, streams)
  - OAuth refresh, cooldown, and credential rotation
  - the context/compact limits handed to Claude Code and where they came from

It never logs credentials, refresh tokens, or request/response bodies. Use it
to diagnose intermittent "Network error" messages without leaking secrets.

Show status:
  ccl debug

Enable or disable:
  ccl debug on
  ccl debug off

When a ccl-launched Claude session exits with debug enabled, ccl prints:
  [ccl debug] session ended · log: <path>
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
			printDebugLogLocation(out)
		} else {
			fmt.Fprintln(out, "Enable with: ccl debug on  (one log per session in ~/.ccl/logs; CCL_DEBUG_LOG overrides)")
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
		printDebugLogLocation(out)
		fmt.Fprintln(out, "Run a ccl session; its log path is printed when the session ends.")
	}
	return nil
}

// printDebugLogLocation explains where session logs land: one file per session,
// derived from the configured base path.
func printDebugLogLocation(out io.Writer) {
	base := oauthproxy.ResolveDebugLogPath()
	extension := filepath.Ext(base)
	fmt.Fprintf(out, "Log base: %s\n", base)
	fmt.Fprintf(out, "Per session: %s-claude_<id>%s\n", strings.TrimSuffix(base, extension), extension)
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
