package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/spf13/cobra"
)

var logCmd = newLogCommand()

func newLogCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "log [on|off]",
		Aliases: []string{"debug"}, // compatibility for scripts written before ccl log
		Short:   "Configure ccl's per-session runtime logs",
		Long: `Configure the threshold for ccl's per-session runtime logs.

The filename template is ~/.ccl/logs/ccl-debug.log by default (override with
CCL_LOG_FILE). That shared file is not created: every ccl-launched Claude Code
session or temporary provider runtime receives a suffixed file, for example
ccl-debug-claude_<id>.log. All slog levels for that session share one file.

Levels:
  on / info  Normal runtime events, including startup, model routing, upstream
             status errors, OAuth refreshes, cooldowns, and context settings.
  debug      Includes INFO plus request metadata. Responses compatibility,
             Copilot, and Kiro runtimes also record final upstream request
             payloads and failed response bodies. These can contain prompts,
             tool results, and user-provided secrets; keep this log local.
  warn/error Show only records emitted at those severities.
  off        Disable file logging.

Show status:
  ccl log

Configure logging:
  ccl log on
  ccl log --level debug
  ccl log off
`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := cmd.Flags().GetString("level")
			if err != nil {
				return err
			}
			return runLog(cmd.OutOrStdout(), args, level)
		},
	}
}

func runLog(out io.Writer, args []string, levelFlag string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}

	levelFlag = strings.TrimSpace(levelFlag)
	if len(args) == 0 && levelFlag == "" {
		level := configuredLogLevel(cfg.LogLevel)
		fmt.Fprintf(out, "Configured log level = %s\n", level)
		if level == oauthproxy.LogLevelOff {
			fmt.Fprintln(out, "Enable with: ccl log on  (one file per Claude session under ~/.ccl/logs; CCL_LOG_FILE overrides)")
			return nil
		}
		printLogLocation(out)
		if level == oauthproxy.LogLevelDebug {
			fmt.Fprintln(out, "DEBUG payload tracing is enabled; this log may contain sensitive conversation data.")
		}
		return nil
	}

	if len(args) > 0 && levelFlag != "" {
		return fmt.Errorf("use either on/off or --level, not both")
	}

	var level oauthproxy.LogLevel
	var ok bool
	if levelFlag != "" {
		level, ok = oauthproxy.ParseLogLevel(levelFlag)
		if !ok || level == oauthproxy.LogLevelOff {
			return fmt.Errorf("expected --level debug, info, warn, or error, got %q", levelFlag)
		}
	} else {
		level, ok = parseLogToggle(args[0])
		if !ok {
			return fmt.Errorf("expected on or off, got %q", args[0])
		}
	}
	cfg.LogLevel = string(level)
	// Clear the pre-log-command settings when a user explicitly changes the
	// level, so config.yaml does not retain two competing representations.
	cfg.DebugMode = false
	cfg.DebugVerbose = false
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}

	oauthproxy.ConfigureLogLevel(level)
	fmt.Fprintf(out, "Configured log level = %s\n", level)
	if level != oauthproxy.LogLevelOff {
		printLogLocation(out)
		if level == oauthproxy.LogLevelDebug {
			fmt.Fprintln(out, "DEBUG payload tracing is enabled; this log may contain sensitive conversation data.")
		}
	}
	return nil
}

func configuredLogLevel(raw string) oauthproxy.LogLevel {
	if level, ok := oauthproxy.ParseLogLevel(raw); ok {
		return level
	}
	return oauthproxy.LogLevelOff
}

func parseLogToggle(raw string) (oauthproxy.LogLevel, bool) {
	if strings.EqualFold(strings.TrimSpace(raw), "on") {
		return oauthproxy.LogLevelInfo, true
	}
	if strings.EqualFold(strings.TrimSpace(raw), "off") {
		return oauthproxy.LogLevelOff, true
	}
	return oauthproxy.LogLevelOff, false
}

func printLogLocation(out io.Writer) {
	base := oauthproxy.ResolveLogTemplatePath()
	extension := filepath.Ext(base)
	fmt.Fprintf(out, "Filename template: %s (not created)\n", base)
	fmt.Fprintf(out, "Per session: %s-claude_<id>%s\n", strings.TrimSuffix(base, extension), extension)
}

func init() {
	logCmd.Flags().String("level", "", "log threshold: debug, info, warn, or error")
	rootCmd.AddCommand(logCmd)
}
