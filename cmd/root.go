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
  ccl ls / ccl use [name]     List or switch providers
  ccl set [name]              Add/update an API-key or OAuth provider (TUI)
  ccl oauth <gpt|grok|workbuddy|...>
                              Log in with a subscription account
  ccl doctor                  Environment + provider + subscription health
  ccl cloud login|push|pull   Encrypted multi-remote config sync
  ccl log on|off              Configure per-session logs (use ccl log --level debug for payload tracing)

Compatibility aliases still work for older scripts:
  ccl auth ...        → ccl oauth ...
  ccl login/push/...  → ccl cloud login/push/...

Run "ccl <command> --help" for details. Extra args after ccl are passed through
to Claude Code (for example: ccl resume, ccl -p "hello").
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runClaude(args)
	},
}

func Execute() {
	configureLogging()

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

// configureLogging makes the persisted threshold available without opening a
// shared file. A Claude session or temporary provider runtime opens its own.
func configureLogging() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
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
