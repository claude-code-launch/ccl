package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var providerCmd = &cobra.Command{
	Use:   "provider",
	Short: "Manage providers",
	Long:  "Manage providers: set, ls, use, cp, mv, rm, or preview injected settings.",
}

var cpCmd = newProviderCopyCommand("cp <source> <target>")
var mvCmd = newProviderMoveCommand("mv <source> <target>")
var rmCmd = newProviderRemoveCommand("rm <name>")

func newProviderSetCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Add or update an LLM provider configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunProviderSet(args)
		},
	}
}

func newProviderUseCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Switch the active provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderUse(args[0])
		},
	}
}

func newProviderPreviewCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Preview the settings JSON for the active provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreview()
		},
	}
}

func newProviderCopyCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Copy a provider configuration",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderCopy(args[0], args[1])
		},
	}
}

func newProviderRemoveCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Delete a provider configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderRemove(args[0])
		},
	}
}

func newProviderMoveCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Rename a provider configuration",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderMove(args[0], args[1])
		},
	}
}

func runProviderUse(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	target := strings.TrimSpace(name)
	if _, exists := cfg.Providers[target]; !exists {
		return fmt.Errorf("provider %q not found in configuration. Add it first using 'ccl set' or check spelling with 'ccl ls'", target)
	}

	cfg.ActiveProvider = target
	err = config.Save(cfg)
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Switched to active provider: %s\n", target)
	return nil
}

func runProviderCopy(sourceName, targetName string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	source := strings.TrimSpace(sourceName)
	target := strings.TrimSpace(targetName)

	if source == "" || target == "" {
		return fmt.Errorf("%s", locale.T("请提供源和目标名称，例如: ccl cp <source> <target>", "please provide both source and target names, e.g.: ccl cp <source> <target>"))
	}
	if source == target {
		return fmt.Errorf("%s", locale.T("源和目标名称不能相同", "source and target must be different"))
	}

	srcProvider, exists := cfg.Providers[source]
	if !exists {
		return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found"), source)
	}

	if _, exists := cfg.Providers[target]; exists {
		fmt.Print(locale.Tf("Provider %q 已存在，是否覆盖？(y/N): ", "Provider %q already exists. Overwrite? (y/N): ", target))
		var answer string
		fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println(locale.T("已取消复制。", "Copy cancelled."))
			return nil
		}
	}

	newP := cloneProvider(srcProvider, target)
	cfg.Providers[target] = newP
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q → %q\n", locale.T("已复制 Provider", "Successfully copied provider"), source, target)
	return nil
}

func runProviderRemove(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	targetName := strings.TrimSpace(name)
	if targetName == "" {
		return fmt.Errorf("%s", locale.T("请指定要删除的 Provider 名称，例如: ccl rm <name>", "please specify the provider name to delete, e.g.: ccl rm <name>"))
	}

	if _, exists := cfg.Providers[targetName]; !exists {
		return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), targetName)
	}

	fmt.Print(locale.Tf("确定要删除 Provider %q？(y/N): ", "Are you sure you want to delete provider %q? (y/N): ", targetName))
	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Println(locale.T("已取消删除。", "Deletion cancelled."))
		return nil
	}

	delete(cfg.Providers, targetName)

	if cfg.ActiveProvider == targetName {
		cfg.ActiveProvider = ""
		if len(cfg.Providers) > 0 {
			var remaining []string
			for name := range cfg.Providers {
				remaining = append(remaining, name)
			}
			sort.Strings(remaining)
			cfg.ActiveProvider = remaining[0]
			fmt.Printf(locale.T("当前 Provider 已重置，切换到 %q\n", "Active provider reset. Switched to %q\n"), cfg.ActiveProvider)
		} else {
			fmt.Println(locale.T("当前 Provider 已清空。", "Active provider cleared."))
		}
	}

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q\n", locale.T("已删除 Provider", "Successfully deleted provider"), targetName)
	return nil
}

func runProviderMove(sourceName, targetName string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	source := strings.TrimSpace(sourceName)
	target := strings.TrimSpace(targetName)

	if source == "" || target == "" {
		return fmt.Errorf("%s", locale.T("请提供旧名称和新名称，例如: ccl mv <source> <target>", "please provide both source and target names, e.g.: ccl mv <source> <target>"))
	}
	if source == target {
		fmt.Println(locale.T("源和目标名称相同，无需操作。", "Source and target are the same, nothing to do."))
		return nil
	}

	if _, exists := cfg.Providers[source]; !exists {
		return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found"), source)
	}

	if _, exists := cfg.Providers[target]; exists {
		fmt.Print(locale.Tf("Provider %q 已存在，是否覆盖？(y/N): ", "Provider %q already exists. Overwrite? (y/N): ", target))
		var answer string
		fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println(locale.T("已取消重命名。", "Rename cancelled."))
			return nil
		}
		delete(cfg.Providers, target)
	}

	cfg.Providers[target] = cloneProvider(cfg.Providers[source], target)
	delete(cfg.Providers, source)

	if cfg.ActiveProvider == source {
		cfg.ActiveProvider = target
	}

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q → %q\n", locale.T("已重命名 Provider", "Successfully renamed provider"), source, target)
	return nil
}

func cloneProvider(p provider.Provider, name string) provider.Provider {
	cloned := p
	cloned.Name = name
	if p.Env != nil {
		cloned.Env = make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			cloned.Env[k] = v
		}
	}
	return cloned
}

func init() {
	providerCmd.AddCommand(
		newProviderSetCommand("set [name]"),
		newProviderListCommand("ls"),
		newProviderUseCommand("use [provider]"),
		newProviderCopyCommand("cp <source> <target>"),
		newProviderMoveCommand("mv <source> <target>"),
		newProviderRemoveCommand("rm <name>"),
		newProviderPreviewCommand("preview"),
	)
	rootCmd.AddCommand(providerCmd, cpCmd, mvCmd, rmCmd)
}
