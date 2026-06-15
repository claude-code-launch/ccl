package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "ccl",
	Short: "ccl is a multi-provider launcher for Claude Code",
	Long:  `ccl manages different LLM providers for Claude Code and runs Claude Code with injected configurations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !IsInstalled() {
			err := AutoInstall()
			if err != nil {
				return err
			}
		}

		p, err := resolveProvider()
		if err != nil {
			return err
		}

		return claude.Run(p, args)
	},
}

func Execute() {
	if len(os.Args) > 1 {
		firstArg := os.Args[1]
		isCclCmd := false
		switch firstArg {
		case "use", "set", "list", "doctor", "settings", "models", "run", "help", "completion", "-h", "--help", "version", "del", "env":
			isCclCmd = true
		}

		if !isCclCmd {
			if !IsInstalled() {
				err := AutoInstall()
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
			}

			p, err := resolveProvider()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			var argsToPass []string
			if firstArg == "claude" {
				argsToPass = os.Args[2:]
			} else {
				argsToPass = os.Args[1:]
			}

			if err := claude.Run(p, argsToPass); err != nil {
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
		os.Exit(1)
	}
}

// resolveProvider determines the active provider.
// Config takes priority over environment variables — once a user has an active_provider
// in config.yaml, stale env vars (like leftover ANTHROPIC_API_KEY from a previous session)
// should not override it. Env vars are only used as a fallback when there is no config.
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
	envAnthropicBase := os.Getenv("ANTHROPIC_BASE_URL")

	if envAnthropicKey != "" {
		p := provider.Provider{
			Name:     "environment-anthropic",
			Type:     "anthropic",
			Endpoint: envAnthropicBase,
			APIKey:   envAnthropicKey,
			// Model:    os.Getenv("ANTHROx[PIC_MODEL"),
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
			p.Endpoint = "https://api.openai.com"
		}
		return p, nil
	}

	return provider.Provider{}, fmt.Errorf("no active provider selected. Use 'ccl add' or 'ccl use', or set OPENAI_API_KEY / ANTHROPIC_API_KEY in environment")
}
