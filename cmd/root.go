package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "ccl",
	Short: "Multi-provider launcher for Claude Code",
	Long: `ccl launches Claude Code with the active provider from ~/.ccl/config.yaml.

Common commands:
  ccl                         Start Claude Code with the active provider
  ccl ls                      List providers and normal/ACP selections
  ccl use [name]              Switch the normal-mode provider; ccl use default
                              runs Claude Code's own configuration (claude.ai)
  ccl use --acp [name]        Select a shared provider configuration for ACP
  ccl set [name]              Add/update an API-key or OAuth provider (TUI)
  ccl proxy -p <name>         Connect direct claude invocations to a background proxy
  ccl oauth <gpt|grok|workbuddy|...>
                              Log in with a subscription account
  ccl doctor                  Environment + provider + subscription health
  ccl acp                     Agent Client Protocol on stdio (Xcode 27 and other ACP clients)
  ccl web                     Conversations and provider settings in a browser
  ccl cloud login|push|pull   Encrypted multi-remote config sync
  ccl sign on|off             Claude's commit/PR signature (off by default; --default also edits ~/.claude/settings.json)
  ccl log on|off              Configure per-session logs (use ccl log --level debug for payload tracing)

Deprecated root shortcuts (removed in the next minor release):
  ccl login/push/pull/status/...  → ccl cloud login/push/pull/status/...
  ccl cp/mv/env/preview/models    → ccl provider cp/mv/env/preview/models

Run "ccl <command> --help" for details. Extra args after ccl are passed through
to Claude Code (for example: ccl resume, ccl -p "hello"). ccl doctor and
ccl update are ccl's own; reach Claude Code's with ccl claude doctor and
ccl claude update.
`,
	// Execute prints every error once; cobra printing it as well duplicated it.
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now: a launch failure such as a missing
		// Claude Code needs its own message, not the command usage.
		cmd.SilenceUsage = true
		return runClaude(args)
	},
}

func Execute() {
	// The status line runs on every Claude Code refresh, so it is answered before
	// anything else: no config load (which can rewrite the file), no logging
	// setup, and no chance of falling through to a billed Claude session.
	if len(os.Args) > 1 && os.Args[1] == "statusline" {
		runStatusline()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		acpQuietErrors = true // Include flag/argument errors before PreRun.
	}
	// Every claude lookup (launch, default, doctor) must see an install
	// the official installer left outside PATH.
	localClaudeDir = useLocalClaudeInstall()

	configureLogging()
	// ccl proxy start/stop recover under their own lifecycle lock; serve is
	// the proxy itself.
	if len(os.Args) < 2 || os.Args[1] != "proxy" {
		recoverStaleProxy()
	}

	if len(os.Args) > 1 {
		firstArg := os.Args[1]

		if !isCclCommand(firstArg) {
			var argsToPass []string
			if firstArg == "claude" {
				argsToPass = os.Args[2:]
			} else {
				argsToPass = os.Args[1:]
			}

			if err := runClaude(argsToPass); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					os.Exit(exitErr.ExitCode())
				}
				fmt.Println(err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		if acpQuietErrors {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Println(err)
		}
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) {
			os.Exit(exitCoder.ExitCode())
		}
		os.Exit(1)
	}
}

// configureLogging makes the persisted threshold available without opening a
// shared file. A Claude session or temporary provider runtime opens its own.
func configureLogging() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
	// Every launch path (normal, proxy serve, ACP) reads the same switch.
	claude.ConfigureSignOn(cfg.SignOn)
}

func isCclCommand(arg string) bool {
	switch arg {
	// --version asks about ccl, so it cannot fall through to Claude Code: that
	// path starts a real session. -v is deliberately not listed, because Claude
	// Code may use it and `ccl -v` should keep reaching it.
	case "help", "completion", "-h", "--help", "--version":
		return true
	}

	for _, command := range rootCmd.Commands() {
		if command.Name() == arg {
			return true
		}
		for _, alias := range command.Aliases {
			if alias == arg {
				return true
			}
		}
	}

	return false
}

func runClaude(args []string) error {
	if localClaudeDir != "" {
		printLocalClaudeHint(os.Stderr, localClaudeDir)
	}
	if !IsInstalled() {
		if err := AutoInstall(); err != nil {
			return err
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config for launcher options: %w", err)
	}
	if provider.IsDefaultProvider(cfg.ActiveProvider) {
		return runClaudeWithoutProvider(applyBypassMode(args, cfg.BypassMode))
	}
	p, err := resolveProvider()
	if err != nil {
		return err
	}

	// Record the configured threshold; claude.Run opens the uniquely named file
	// before its embedded runtime starts.
	level := configuredLogLevel(cfg.LogLevel)
	oauthproxy.ConfigureLogLevel(level)

	err = claude.Run(p, applyBypassMode(args, cfg.BypassMode))
	if logPath := oauthproxy.LogFilePath(); logPath != "" {
		fmt.Fprintf(os.Stderr, "\n[ccl log] run finished · file: %s\n", logPath)
		oauthproxy.CloseLog()
	}
	return err
}

// resolveProvider returns the active provider. ccl no longer builds an
// implicit provider from OPENAI_API_KEY / ANTHROPIC_API_KEY in the shell: a key
// exported for another tool would quietly bill a Claude Code session. Without
// a selection, the user picks one, or runs Claude Code on its own login with
// `ccl use default`.
func resolveProvider() (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.ActiveProvider == "" {
		return provider.Provider{}, errors.New(locale.T(
			"未选择 Provider。用 ccl set 添加、ccl use <name> 选择，或用 ccl use default 直接以 Claude Code 自身登录启动",
			"no provider selected. Add one with ccl set, choose one with ccl use <name>, or run Claude Code on its own login with ccl use default",
		))
	}
	p, ok := cfg.Providers[cfg.ActiveProvider]
	if !ok {
		return provider.Provider{}, fmt.Errorf("active provider %q not found in configuration", cfg.ActiveProvider)
	}
	return p, nil
}
