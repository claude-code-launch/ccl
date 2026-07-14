package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

var envCmd = newEnvCommand("env [KEY VALUE | ls | rm KEY | mv OLD NEW]")

// newEnvCommand manages environment variables for the active provider.
func newEnvCommand(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Manage environment variables",
		Long: `Manage environment variables for the active provider.

Set or modify a variable:
  ccl env KEY VALUE
  ccl provider env KEY VALUE

List all variables:
  ccl env ls
  ccl provider env ls

Delete a variable:
  ccl env rm KEY
  ccl provider env rm KEY

Rename a variable:
  ccl env mv OLD_KEY NEW_KEY
  ccl provider env mv OLD_KEY NEW_KEY
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvSet(args)
		},
	}
	cmd.AddCommand(newEnvListCommand(), newEnvRemoveCommand(), newEnvMoveCommand())
	return cmd
}

func runEnvSet(args []string) error {
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
	if key == claude.MaxOutputTokensEnv {
		normalized, err := claude.NormalizeMaxOutputTokens(val)
		if err != nil {
			return fmt.Errorf("%s %w", key, err)
		}
		val = normalized
	}

	p.Env[key] = val
	cfg.Providers[cfg.ActiveProvider] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s=%s\n", key, val)
	return nil
}

// newEnvListCommand lists environment variables.
func newEnvListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all environment variables",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvList()
		},
	}
}

func runEnvList() error {
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
}

// newEnvRemoveCommand deletes an environment variable.
func newEnvRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rm KEY",
		Short: "Delete an environment variable",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvRemove(args[0])
		},
	}
}

func runEnvRemove(arg string) error {
	key := strings.TrimSpace(arg)
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

	fmt.Printf(locale.T("确定要删除 %s 吗？(y/N): ", "Delete %s? (y/N): "), key)
	var confirmStr string
	fmt.Scanln(&confirmStr)
	confirmStr = strings.ToLower(strings.TrimSpace(confirmStr))
	if confirmStr != "y" && confirmStr != "yes" {
		fmt.Println(locale.T("已取消。", "Cancelled."))
		return nil
	}

	delete(p.Env, key)
	cfg.Providers[cfg.ActiveProvider] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ Deleted %s\n", key)
	return nil
}

// newEnvMoveCommand renames an environment variable.
func newEnvMoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mv OLD_KEY NEW_KEY",
		Short: "Rename an environment variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvMove(args[0], args[1])
		},
	}
}

func runEnvMove(oldArg, newArg string) error {
	oldKey := strings.TrimSpace(oldArg)
	newKey := strings.TrimSpace(newArg)
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
		fmt.Printf(locale.T("键 %s 已存在，是否覆盖？(y/N): ", "Key %s already exists. Overwrite? (y/N): "), newKey)
		var confirmStr string
		fmt.Scanln(&confirmStr)
		confirmStr = strings.ToLower(strings.TrimSpace(confirmStr))
		if confirmStr != "y" && confirmStr != "yes" {
			fmt.Println(locale.T("已取消。", "Cancelled."))
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
}

func init() {
	rootCmd.AddCommand(envCmd)
}
