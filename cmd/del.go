package cmd

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/spf13/cobra"
)

var delCmd = &cobra.Command{
	Use:   "del [provider]",
	Short: "Delete an LLM provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		var targetName string
		if len(args) > 0 {
			targetName = strings.TrimSpace(args[0])
		}

		if targetName == "" {
			if len(cfg.Providers) == 0 {
				fmt.Println("No providers configured to delete.")
				return nil
			}

			var options []huh.Option[string]
			var names []string
			for name := range cfg.Providers {
				names = append(names, name)
			}
			sort.Strings(names)

			for _, name := range names {
				label := name
				if name == cfg.ActiveProvider {
					label = fmt.Sprintf("%s (active)", name)
				}
				options = append(options, huh.NewOption(label, name))
			}

			err = huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Select Provider to Delete").
						Options(options...).
						Value(&targetName),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		if targetName == "" {
			return nil
		}

		if _, exists := cfg.Providers[targetName]; !exists {
			return fmt.Errorf("provider %q not found in configuration", targetName)
		}

		// Confirm deletion
		var confirm bool
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Are you sure you want to delete provider %q?", targetName)).
					Value(&confirm),
			),
		).Run()
		if err != nil {
			return err
		}

		if !confirm {
			fmt.Println("Deletion cancelled.")
			return nil
		}

		delete(cfg.Providers, targetName)

		if cfg.ActiveProvider == targetName {
			cfg.ActiveProvider = ""
			// If there are other providers, auto-select one
			if len(cfg.Providers) > 0 {
				var remaining []string
				for name := range cfg.Providers {
					remaining = append(remaining, name)
				}
				sort.Strings(remaining)
				cfg.ActiveProvider = remaining[0]
				fmt.Printf("Active provider reset. Switched to %q\n", cfg.ActiveProvider)
			} else {
				fmt.Println("Active provider cleared.")
			}
		}

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ Successfully deleted provider %q\n", targetName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(delCmd)
}
