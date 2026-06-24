package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/spf13/cobra"
)

// confCmd handles provider configuration tasks.
// Without a subcommand it behaves like "ccl conf set".
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
	Long:  setCmd.Long,
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunConfSet(args)
	},
}

// confLsCmd lists all registered providers.
	var lsShowAll bool

var confLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List all registered providers",
	RunE: func(cmd *cobra.Command, args []string) error {
				lsShowAll = cmd.Flags().Lookup("all").Changed
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if len(cfg.Providers) == 0 {
			fmt.Println(lang.T("还没有配置 Provider。使用 'ccl conf set' 添加一个。", "No providers configured yet. Use 'ccl conf set' to add one."))
			return nil
		}

		var names []string
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)

		fmt.Println(lang.T("已注册的 Provider：", "Registered providers:"))
		for _, name := range names {
			mark := " "
			if name == cfg.ActiveProvider {
				mark = "*"
			}
			p := cfg.Providers[name]
			fmt.Printf("%s %s (%s, model: %s)\n", mark, name, p.Type, formatModelList(p.Model, lsShowAll))
		}

		return nil
	},
}

// confCpCmd copies a provider configuration from source to target.
var confCpCmd = &cobra.Command{
	Use:   "cp <source> <target>",
	Short: "Copy a provider configuration",
	Args:  cobra.RangeArgs(0, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		var source, target string

		switch len(args) {
		case 0:
			// Interactive: pick source, then input target name
			if len(cfg.Providers) == 0 {
				fmt.Println(lang.T("还没有配置的 Provider 可以复制。", "No providers configured to copy."))
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
						Title(lang.T("选择要复制的 Provider", "Select Provider to Copy")).
						Options(options...).
						Value(&source),
				),
			).Run()
			if err != nil {
				return err
			}
			if source == "" {
				return nil
			}

			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(lang.T("目标名称", "Target Name")).
						Description(lang.T("复制后的 Provider 名称", "Name for the copied provider")).
						Value(&target).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New("target name cannot be empty")
							}
							return nil
						}),
				),
			).Run()
			if err != nil {
				return err
			}
			target = strings.TrimSpace(target)

		case 1:
			source = strings.TrimSpace(args[0])
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(lang.T("目标名称", "Target Name")).
						Description(lang.T("复制后的 Provider 名称", "Name for the copied provider")).
						Value(&target).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New("target name cannot be empty")
							}
							return nil
						}),
				),
			).Run()
			if err != nil {
				return err
			}
			target = strings.TrimSpace(target)

		case 2:
			source = strings.TrimSpace(args[0])
			target = strings.TrimSpace(args[1])
		}

		if source == "" || target == "" {
			return nil
		}

		if source == target {
			return fmt.Errorf(lang.T("源和目标名称不能相同", "source and target must be different"))
		}

		srcProvider, exists := cfg.Providers[source]
		if !exists {
			return fmt.Errorf(lang.T("未找到 Provider %q", "provider %q not found"), source)
		}

		if _, exists := cfg.Providers[target]; exists {
			var overwrite bool
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(lang.Tf("Provider %q 已存在，是否覆盖？", "Provider %q already exists. Overwrite?", target)).
						Value(&overwrite),
				),
			).Run()
			if err != nil {
				return err
			}
			if !overwrite {
				fmt.Println(lang.T("已取消复制。", "Copy cancelled."))
				return nil
			}
		}

		// Deep copy by struct assignment (all fields are value types or maps)
		newP := srcProvider
		newP.Name = target

		// Deep copy the Env map to avoid aliasing
		if srcProvider.Env != nil {
			newP.Env = make(map[string]string, len(srcProvider.Env))
			for k, v := range srcProvider.Env {
				newP.Env[k] = v
			}
		}

		cfg.Providers[target] = newP

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

fmt.Printf("✅ %s %q → %q\n", lang.T("已复制 Provider", "Successfully copied provider"), source, target)
		return nil
	},
}

// confRmCmd deletes a provider configuration.
var confRmCmd = &cobra.Command{
	Use:   "rm [name]",
	Short: "Delete a provider configuration",
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
				fmt.Println(lang.T("还没有配置的 Provider 可以删除。", "No providers configured to delete."))
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
						Title(lang.T("选择要删除的 Provider", "Select Provider to Delete")).
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

		var confirm bool
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(lang.Tf("确定要删除 Provider %q？", "Are you sure you want to delete provider %q?", targetName)).
					Value(&confirm),
			),
		).Run()
		if err != nil {
			return err
		}

		if !confirm {
			fmt.Println(lang.T("已取消删除。", "Deletion cancelled."))
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
				fmt.Printf(lang.T("当前 Provider 已重置，切换到 %q\n", "Active provider reset. Switched to %q\n"), cfg.ActiveProvider)
			} else {
				fmt.Println(lang.T("当前 Provider 已清空。", "Active provider cleared."))
			}
		}

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

fmt.Printf("✅ %s %q\n", lang.T("已删除 Provider", "Successfully deleted provider"), targetName)
		return nil
	},
}


// truncateModels shortens a comma-separated model list for clean display.
// formatModelList formats a model list for display.
// When showAll is true, returns the full list; otherwise truncates to 3 models.
func formatModelList(modelStr string, showAll bool) string {
	if showAll {
		return modelStr
	}
	models := strings.Split(modelStr, ",")
	if len(models) <= 3 {
		return modelStr
	}
	return strings.Join(models[:3], ",") + ", …"
}

// confMvCmd renames a provider configuration.
var confMvCmd = &cobra.Command{
	Use:   "mv <source> <target>",
	Short: "Rename a provider configuration",
	Args:  cobra.RangeArgs(0, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		var source, target string

		switch len(args) {
		case 0:
			if len(cfg.Providers) == 0 {
				fmt.Println(lang.T("还没有配置的 Provider 可以重命名。", "No providers configured to rename."))
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
						Title(lang.T("选择要重命名的 Provider", "Select Provider to Rename")).
						Options(options...).
						Value(&source),
				),
			).Run()
			if err != nil {
				return err
			}
			if source == "" {
				return nil
			}

			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(lang.T("新名称", "New Name")).
						Description(lang.T("Provider 的新名称", "New name for the provider")).
						Value(&target).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New("name cannot be empty")
							}
							return nil
						}),
				),
			).Run()
			if err != nil {
				return err
			}
			target = strings.TrimSpace(target)

		case 1:
			source = strings.TrimSpace(args[0])
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(lang.T("新名称", "New Name")).
						Description(lang.T("Provider 的新名称", "New name for the provider")).
						Value(&target).
						Validate(func(str string) error {
							if strings.TrimSpace(str) == "" {
								return errors.New("name cannot be empty")
							}
							return nil
						}),
				),
			).Run()
			if err != nil {
				return err
			}
			target = strings.TrimSpace(target)

		case 2:
			source = strings.TrimSpace(args[0])
			target = strings.TrimSpace(args[1])
		}

		if source == "" || target == "" {
			return nil
		}

		if source == target {
			fmt.Println(lang.T("源和目标名称相同，无需操作。", "Source and target are the same, nothing to do."))
			return nil
		}

		if _, exists := cfg.Providers[source]; !exists {
			return fmt.Errorf(lang.T("未找到 Provider %q", "provider %q not found"), source)
		}

		if _, exists := cfg.Providers[target]; exists {
			var overwrite bool
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(lang.Tf("Provider %q 已存在，是否覆盖？", "Provider %q already exists. Overwrite?", target)).
						Value(&overwrite),
				),
			).Run()
			if err != nil {
				return err
			}
			if !overwrite {
				fmt.Println(lang.T("已取消重命名。", "Rename cancelled."))
				return nil
			}
			delete(cfg.Providers, target)
		}

		// Deep copy
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

		// Update active if renamed
		if cfg.ActiveProvider == source {
			cfg.ActiveProvider = target
		}

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

fmt.Printf("✅ %s %q → %q\n", lang.T("已重命名 Provider", "Successfully renamed"), source, target)
		return nil
	},
}

func init() {
	confLsCmd.Flags().BoolVarP(&lsShowAll, "all", "a", false, "Show all models without truncation")
	confCmd.AddCommand(confSetCmd, confLsCmd, confCpCmd, confMvCmd, confRmCmd)
	rootCmd.AddCommand(confCmd)
}
