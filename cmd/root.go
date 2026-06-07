package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/cc/internal/claude"
	"github.com/haiboyuwen/cc/internal/config"
)

var rootCmd = &cobra.Command{
	Use:   "cc",
	Short: "cc is a multi-provider launcher for Claude Code",
	Long:  `cc manages different LLM providers for Claude Code and runs Claude Code with injected configurations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if Claude Code is installed first
		if !claude.IsInstalled() {
			err := claude.AutoInstall()
			if err != nil {
				return err
			}
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if cfg.ActiveProvider == "" {
			return fmt.Errorf("no active provider selected. Use 'cc add' or 'cc use [provider]' first")
		}

		p, ok := cfg.Providers[cfg.ActiveProvider]
		if !ok {
			return fmt.Errorf("active provider %q not found in configuration", cfg.ActiveProvider)
		}

		return claude.Run(p, args)
	},
}

func Execute() {
	// We want to pass any unrecognized flags directly to Claude, but Cobra normally parses flags.
	// We will allow unknown flags so they can be forwarded as arguments to Run.
	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
