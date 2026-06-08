package cmd

import (
	"fmt"
	"sort"

	"github.com/haiboyuwen/claude-code-launch/internal/config"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if len(cfg.Providers) == 0 {
			fmt.Println("No providers added yet. Use 'ccl add' to add one.")
			return nil
		}

		// Sort provider names for consistent output
		var names []string
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)

		fmt.Println("Registered providers:")
		for _, name := range names {
			mark := " "
			if name == cfg.ActiveProvider {
				mark = "*"
			}
			p := cfg.Providers[name]
			fmt.Printf("%s %s (%s, model: %s)\n", mark, name, p.Type, p.Model)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
