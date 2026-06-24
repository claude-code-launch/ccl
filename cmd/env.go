package cmd

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/spf13/cobra"
)

// envCmd manages environment variables for the active provider.
// Without a subcommand it expects "KEY VALUE" args to set/modify a variable.
var envCmd = &cobra.Command{
	Use:   "env [KEY VALUE | ls | rm KEY | mv OLD NEW]",
	Short: "Manage environment variables",
	Long: `Manage environment variables for the active provider.

Set or modify a variable:
  ccl env KEY VALUE

List all variables:
  ccl env ls

Delete a variable:
  ccl env rm KEY

Rename a variable:
  ccl env mv OLD_KEY NEW_KEY
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// "ccl env KEY VALUE" — set/modify
		if len(args) != 2 {
			return fmt.Errorf("expected KEY and VALUE arguments, or a subcommand (ls, rm, mv). See ccl env --help")
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if cfg.ActiveProvider == "" {
			return fmt.Errorf("no active provider set. Use 'ccl set' or 'ccl use' first")
		}

		p := cfg.Providers[cfg.ActiveProvider]
		if p.Env == nil {
			p.Env = make(map[string]string)
		}

		key := strings.TrimSpace(args[0])
		val := strings.TrimSpace(args[1])
		if key == "" {
			return fmt.Errorf("key cannot be empty")
		}

		p.Env[key] = val
		cfg.Providers[cfg.ActiveProvider] = p
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ %s=%s\n", key, val)
		return nil
	},
}

// envLsCmd lists all environment variables for the active provider.
var envLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List all environment variables",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		if cfg.ActiveProvider == "" {
			fmt.Println("No active provider set.")
			return nil
		}

		p, exists := cfg.Providers[cfg.ActiveProvider]
		if !exists {
			return fmt.Errorf("active provider %q not found", cfg.ActiveProvider)
		}

		if len(p.Env) == 0 {
			fmt.Printf("No environment variables configured for %q.\n", cfg.ActiveProvider)
			return nil
		}

		var keys []string
		for k := range p.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		fmt.Printf("Environment variables for %q:\n", cfg.ActiveProvider)
		for _, k := range keys {
			fmt.Printf("  %s=%s\n", k, p.Env[k])
		}
		return nil
	},
}

// envRmCmd deletes an environment variable.
var envRmCmd = &cobra.Command{
	Use:   "rm KEY",
	Short: "Delete an environment variable",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := strings.TrimSpace(args[0])
		if key == "" {
			return fmt.Errorf("key cannot be empty")
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		if cfg.ActiveProvider == "" {
			return fmt.Errorf("no active provider set")
		}

		p, exists := cfg.Providers[cfg.ActiveProvider]
		if !exists {
			return fmt.Errorf("active provider %q not found", cfg.ActiveProvider)
		}

		if _, exists := p.Env[key]; !exists {
			return fmt.Errorf("key %q not found in %q", key, cfg.ActiveProvider)
		}

		var confirm bool
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Delete %s from %q?", key, cfg.ActiveProvider)).
					Value(&confirm),
			),
		).Run()
		if err != nil {
			return err
		}
		if !confirm {
			fmt.Println("Cancelled.")
			return nil
		}

		delete(p.Env, key)
		cfg.Providers[cfg.ActiveProvider] = p
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ Deleted %s\n", key)
		return nil
	},
}

// envMvCmd renames an environment variable.
var envMvCmd = &cobra.Command{
	Use:   "mv OLD_KEY NEW_KEY",
	Short: "Rename an environment variable",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		oldKey := strings.TrimSpace(args[0])
		newKey := strings.TrimSpace(args[1])
		if oldKey == "" || newKey == "" {
			return fmt.Errorf("keys cannot be empty")
		}
		if oldKey == newKey {
			fmt.Println("Old and new keys are the same, nothing to do.")
			return nil
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		if cfg.ActiveProvider == "" {
			return fmt.Errorf("no active provider set")
		}

		p, exists := cfg.Providers[cfg.ActiveProvider]
		if !exists {
			return fmt.Errorf("active provider %q not found", cfg.ActiveProvider)
		}

		val, exists := p.Env[oldKey]
		if !exists {
			return fmt.Errorf("key %q not found in %q", oldKey, cfg.ActiveProvider)
		}

		if _, exists := p.Env[newKey]; exists {
			var overwrite bool
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(fmt.Sprintf("Key %q already exists. Overwrite with value from %q?", newKey, oldKey)).
						Value(&overwrite),
				),
			).Run()
			if err != nil {
				return err
			}
			if !overwrite {
				fmt.Println("Cancelled.")
				return nil
			}
		}

		delete(p.Env, oldKey)
		p.Env[newKey] = val
		cfg.Providers[cfg.ActiveProvider] = p
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ Renamed %s → %s\n", oldKey, newKey)
		return nil
	},
}

func init() {
	envCmd.AddCommand(envLsCmd, envRmCmd, envMvCmd)
	rootCmd.AddCommand(envCmd)
}
