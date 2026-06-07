package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/haiboyuwen/cc/internal/config"
)

var useCmd = &cobra.Command{
	Use:   "use [provider]",
	Short: "Switch the active provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		target := args[0]
		if _, exists := cfg.Providers[target]; !exists {
			return fmt.Errorf("provider %q not found in configuration. Add it first using 'ccl add' or check spelling with 'ccl list'", target)
		}

		cfg.ActiveProvider = target
		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("Switched to active provider: %s\n", target)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(useCmd)
}
