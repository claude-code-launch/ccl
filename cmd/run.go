package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/cc/internal/claude"
	"github.com/haiboyuwen/cc/internal/config"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run Claude Code with active provider config directly",
	RunE: func(cmd *cobra.Command, args []string) error {
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

func init() {
	rootCmd.AddCommand(runCmd)
}
