package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

// confCmd handles provider configuration tasks.
var confCmd = &cobra.Command{
	Use:   "conf",
	Short: "Manage provider configurations",
	Long: `Manage provider configurations — list, copy, delete, or create/update.
Without a subcommand, behaves like "ccl conf set": interactively add or update a provider.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunConfSet(args)
	},
}

// confSetCmd explicitly adds/updates a provider (same as "ccl set").
var confSetCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Add or update an LLM provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunConfSet(args)
	},
}

var lsShowAll bool

// confLsCmd lists all registered providers.
var confLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List all registered providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		lsShowAll = cmd.Flags().Lookup("all").Changed
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		return printProviders(
			cmd.OutOrStdout(),
			cfg,
			lsShowAll,
			locale.T("还没有配置 Provider。使用 'ccl conf set' 添加一个。", "No providers configured yet. Use 'ccl conf set' to add one."),
			locale.T("已注册的 Provider：", "Registered providers:"),
		)
	},
}

// confCpCmd copies a provider configuration from source to target.
var confCpCmd = &cobra.Command{
	Use:   "cp <source> <target>",
	Short: "Copy a provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("%s", locale.T("请提供源和目标名称，例如: ccl conf cp <source> <target>", "please provide both source and target names, e.g.: ccl conf cp <source> <target>"))
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		source := strings.TrimSpace(args[0])
		target := strings.TrimSpace(args[1])

		if source == target {
			return fmt.Errorf("%s", locale.T("源和目标名称不能相同", "source and target must be different"))
		}

		srcProvider, exists := cfg.Providers[source]
		if !exists {
			return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found"), source)
		}

		// 检查目标是否存在，若存在，通过标准命令行 I/O 进行覆盖确认
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

		// 深拷贝
		newP := srcProvider
		newP.Name = target

		if srcProvider.Env != nil {
			newP.Env = make(map[string]string, len(srcProvider.Env))
			for k, v := range srcProvider.Env {
				newP.Env[k] = v
			}
		}

		cfg.Providers[target] = newP
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ %s %q → %q\n", locale.T("已复制 Provider", "Successfully copied provider"), source, target)
		return nil
	},
}

// confRmCmd deletes a provider configuration.
var confRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Delete a provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("%s", locale.T("请指定要删除的 Provider 名称，例如: ccl conf rm <name>", "please specify the provider name to delete, e.g.: ccl conf rm <name>"))
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		targetName := strings.TrimSpace(args[0])

		if _, exists := cfg.Providers[targetName]; !exists {
			return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), targetName)
		}

		// 命令行标准双重确认
		fmt.Print(locale.Tf("确定要删除 Provider %q？(y/N): ", "Are you sure you want to delete provider %q? (y/N): ", targetName))
		var answer string
		fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println(locale.T("已取消删除。", "Deletion cancelled."))
			return nil
		}

		delete(cfg.Providers, targetName)

		// 联动重置 Active 逻辑
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
	},
}

// confMvCmd renames a provider configuration.
var confMvCmd = &cobra.Command{
	Use:   "mv <source> <target>",
	Short: "Rename a provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("%s", locale.T("请提供旧名称和新名称，例如: ccl conf mv <source> <target>", "please provide both source and target names, e.g.: ccl conf mv <source> <target>"))
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		source := strings.TrimSpace(args[0])
		target := strings.TrimSpace(args[1])

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

		// 执行改名深拷贝
		srcProvider := cfg.Providers[source]
		newP := srcProvider
		newP.Name = target
		if srcProvider.Env != nil {
			newP.Env = make(map[string]string, len(srcProvider.Env))
			for k, v := range srcProvider.Env {
				newP.Env[k] = v
			}
		}
		cfg.Providers[target] = newP
		delete(cfg.Providers, source)

		if cfg.ActiveProvider == source {
			cfg.ActiveProvider = target
		}

		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✅ %s %q → %q\n", locale.T("已重命名 Provider", "Successfully renamed provider"), source, target)
		return nil
	},
}

// formatModelList formats a model list for display.
func formatModelList(modelStr string, showAll bool) string {
	if showAll || modelStr == "" {
		return modelStr
	}
	models := strings.Split(modelStr, ",")
	if len(models) <= 3 {
		return modelStr
	}
	return strings.Join(models[:3], ",") + ", …"
}

func init() {
	confLsCmd.Flags().BoolVarP(&lsShowAll, "all", "a", false, "Show all models without truncation")
	confCmd.AddCommand(confSetCmd, confLsCmd, confCpCmd, confMvCmd, confRmCmd)
	rootCmd.AddCommand(confCmd)
}
