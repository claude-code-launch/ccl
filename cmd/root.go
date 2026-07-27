package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
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
  ccl ls / ccl use <name>     List or switch providers
  ccl set [name]              Add/update an API-key or OAuth provider (TUI)
  ccl oauth <gpt|grok|...>    Log in with a subscription account
  ccl oauth group / sync      Multi-account pools and credential cleanup
  ccl doctor                  Environment + provider + CPA account health
  ccl cloud login|push|pull   Encrypted multi-remote config sync
  ccl debug on|off            Runtime diagnostics (log path printed when a session ends)

Compatibility aliases still work for older scripts:
  ccl auth ...        → ccl oauth ...
  ccl sync            → ccl oauth sync
  ccl login/push/...  → ccl cloud login/push/...

Run "ccl <command> --help" for details. Extra args after ccl are passed through
to Claude Code (for example: ccl resume, ccl -p "hello").
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runClaude(args)
	},
}

func Execute() {
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
		fmt.Println(err)
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) {
			os.Exit(exitCoder.ExitCode())
		}
		os.Exit(1)
	}
}

func isCclCommand(arg string) bool {
	switch arg {
	case "help", "completion", "-h", "--help":
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
	if !IsInstalled() {
		if err := AutoInstall(); err != nil {
			return err
		}
	}

	p, err := resolveProvider()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config for launcher options: %w", err)
	}

	// Establish the runtime debug sink before the embedded CPA starts so its
	// logrus funnel and startup Debugf see the enabled state. Bypass mode and
	// debug are independent global toggles persisted in config.yaml.
	oauthproxy.SetDebug(cfg.DebugMode, oauthproxy.ResolveDebugLogPath())

	err = claude.Run(p, applyBypassMode(args, cfg.BypassMode))
	if cfg.DebugMode {
		logPath := oauthproxy.DebugLogPath()
		if logPath == "" {
			logPath = oauthproxy.ResolveDebugLogPath()
		}
		fmt.Fprintf(os.Stderr, "\n[ccl debug] session ended · log: %s\n", logPath)
	}
	return err
}

// resolveProvider determines the active provider.
// Config takes priority over environment variables — once a user has an active_provider
// in config.yaml, stale env vars (like leftover ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN from a previous session) should not override it. Env vars
// are only used as a fallback when there is no config.
func resolveProvider() (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}

	// Config takes priority: if active_provider is set, use it
	if cfg.ActiveProvider != "" {
		p, ok := cfg.Providers[cfg.ActiveProvider]
		if !ok {
			return provider.Provider{}, fmt.Errorf("active provider %q not found in configuration", cfg.ActiveProvider)
		}
		return p, nil
	}

	// No config — fallback to environment variables
	envAnthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	envAnthropicAuthToken := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	envAnthropicBase := os.Getenv("ANTHROPIC_BASE_URL")

	if envAnthropicAuthToken != "" || envAnthropicKey != "" {
		apiKey := envAnthropicKey
		anthropicAuth := ""
		if envAnthropicAuthToken != "" {
			apiKey = envAnthropicAuthToken
			anthropicAuth = "bearer"
		}
		p := provider.Provider{
			Name:          "environment-anthropic",
			Type:          "anthropic",
			Endpoint:      envAnthropicBase,
			APIKey:        apiKey,
			Model:         os.Getenv("ANTHROPIC_MODEL"),
			AnthropicAuth: anthropicAuth,
		}
		if p.Endpoint == "" {
			p.Endpoint = "https://api.anthropic.com"
		}
		return p, nil
	}

	envAPIKey := os.Getenv("OPENAI_API_KEY")
	envBaseURL := os.Getenv("OPENAI_BASE_URL")

	if envAPIKey != "" {
		p := provider.Provider{
			Name:     "environment",
			Type:     "openai",
			Endpoint: envBaseURL,
			APIKey:   envAPIKey,
			Model:    os.Getenv("OPENAI_MODEL"),
		}
		if p.Endpoint == "" {
			p.Endpoint = "https://api.openai.com/v1"
		}
		return p, nil
	}

	return provider.Provider{}, fmt.Errorf("no active provider selected. Use 'ccl set' or 'ccl use', or set OPENAI_API_KEY / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN in environment")
}
