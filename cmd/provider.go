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
	Long: `Manage LLM providers stored in ~/.ccl/config.yaml.

Subcommands:
  set [name]     Interactive add/update (TUI)
  ls             List providers
  use [--acp] [name]
                 Switch the provider selected by normal or ACP mode
  cp/mv/rm       Copy, rename, delete
  map            Slot → model mapping (Opus/Sonnet/Haiku/Custom)
  models         Availability check for the model pool
  env            Provider-scoped environment variables
  preview        Show settings JSON injected into Claude Code

Most of these are also available as root shortcuts:
  ccl set / ccl ls / ccl use / ccl map / ccl models / ccl env / ccl preview
`,
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
	cmd := &cobra.Command{
		Use:   use,
		Short: "Switch the provider used by normal or ACP mode",
		Long: `Switch which shared provider configuration a launch mode uses.

Without --acp, the selection applies to normal ccl launches. With --acp, the
same provider entry is selected for ccl acp; no provider settings are copied or
maintained separately.

Examples:
  ccl use cc
  ccl use --acp cc`,
		// Parse the tiny grammar without retaining mutable flag state between
		// invocations of the reusable Cobra root used by embedders and tests.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, forACP, showHelp, err := parseProviderUseArgs(args)
			if err != nil {
				return err
			}
			if showHelp {
				return cmd.Help()
			}
			return runProviderUse(name, forACP)
		},
	}
	// Register the flag for help and completion metadata. Parsing is handled
	// above so repeated in-process command execution does not retain its value.
	cmd.Flags().Bool("acp", false, "Switch the provider used by ACP mode")
	return cmd
}

func parseProviderUseArgs(args []string) (name string, forACP, showHelp bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--acp":
			forACP = true
		case "-h", "--help":
			showHelp = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, false, fmt.Errorf("unknown flag: %s", arg)
			}
			if name != "" {
				return "", false, false, fmt.Errorf("accepts at most 1 provider name, received %q and %q", name, arg)
			}
			name = arg
		}
	}
	return name, forACP, showHelp, nil
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
	// Declared per command because each constructor is called twice: once for
	// the root shortcut and once under `ccl provider`.
	yes := false
	cmd := &cobra.Command{
		Use:   use,
		Short: "Copy a provider configuration",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderCopy(args[0], args[1], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Overwrite an existing target without confirmation")
	return cmd
}

func newProviderRemoveCommand(use string) *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:   use,
		Short: "Delete a provider configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderRemove(args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without confirmation")
	return cmd
}

func newProviderMoveCommand(use string) *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:   use,
		Short: "Rename a provider configuration",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderMove(args[0], args[1], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Overwrite an existing target without confirmation")
	return cmd
}

func runProviderUse(name string, forACP bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	target := strings.TrimSpace(name)
	if target == "" {
		// 没有传 provider name：弹出与 `ccl set` 一致的已有 Provider 过滤列
		// 表，选中即切换；取消则静默退出。
		target, err = selectProviderToUse(cfg, forACP)
		if err != nil {
			return err
		}
		if target == "" {
			return nil
		}
	}

	if _, exists := cfg.Providers[target]; !exists {
		return fmt.Errorf(locale.T("未找到 Provider %q。请先用 'ccl set' 添加，或用 'ccl ls' 检查拼写", "provider %q not found in configuration. Add it first using 'ccl set' or check spelling with 'ccl ls'"), target)
	}

	if forACP {
		cfg.ACPProvider = target
	} else {
		cfg.ActiveProvider = target
	}
	err = config.Save(cfg)
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if forACP {
		fmt.Printf(locale.T("ACP 已切换为使用 Provider 配置：%s\n", "ACP now uses provider configuration: %s\n"), target)
	} else {
		fmt.Printf(locale.T("已切换到激活 Provider：%s\n", "Switched to active provider: %s\n"), target)
	}
	return nil
}

// selectProviderToUse runs the filter selector over the configured providers so
// `ccl use` without arguments can switch interactively, mirroring the selection
// list `ccl set` shows (but without the create-new entry — use only switches
// between existing providers). It returns "" when the user cancels.
func selectProviderToUse(cfg *provider.Config, forACP bool) (string, error) {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("%s", locale.T("没有可用的 Provider，请先用 'ccl set' 添加", "no providers configured. Add one first using 'ccl set'"))
	}
	selected := cfg.ActiveProvider
	if forACP {
		selected = cfg.ACPProvider
	}
	labelFor := func(name string) string {
		if name == selected {
			return fmt.Sprintf("%s %s", name, locale.T("(当前使用)", "(active)"))
		}
		return name
	}
	items := make([]string, 0, len(names))
	for _, name := range names {
		items = append(items, labelFor(name))
	}
	prompt := locale.T("选择普通模式要使用的 Provider:", "Select the provider for normal mode:")
	if forACP {
		prompt = locale.T("选择 ACP 要使用的 Provider:", "Select the provider for ACP mode:")
	}
	chosen, err := runSelect(prompt, items)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if chosen == labelFor(name) {
			return name, nil
		}
	}
	return "", nil
}

// confirmed prints prompt and reports whether the answer was an explicit yes.
// force skips the prompt. Anything else — including EOF, which is what an
// unanswered prompt yields when stdin is not a terminal — counts as no, so a
// scripted invocation without --yes declines instead of destroying anything.
func confirmed(prompt string, force bool) bool {
	if force {
		return true
	}
	fmt.Print(prompt)
	var answer string
	_, _ = fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func runProviderCopy(sourceName, targetName string, force bool) error {
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
		prompt := locale.Tf("Provider %q 已存在，是否覆盖？(y/N): ", "Provider %q already exists. Overwrite? (y/N): ", target)
		if !confirmed(prompt, force) {
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

func runProviderRemove(name string, force bool) error {
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

	prompt := locale.Tf("确定要删除 Provider %q？(y/N): ", "Are you sure you want to delete provider %q? (y/N): ", targetName)
	if !confirmed(prompt, force) {
		fmt.Println(locale.T("已取消删除。", "Deletion cancelled."))
		return nil
	}

	delete(cfg.Providers, targetName)

	replacement := ""
	if len(cfg.Providers) > 0 {
		remaining := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			remaining = append(remaining, name)
		}
		sort.Strings(remaining)
		replacement = remaining[0]
	}
	if cfg.ActiveProvider == targetName {
		cfg.ActiveProvider = replacement
		if replacement != "" {
			fmt.Printf(locale.T("当前 Provider 已重置，切换到 %q\n", "Active provider reset. Switched to %q\n"), replacement)
		} else {
			fmt.Println(locale.T("当前 Provider 已清空。", "Active provider cleared."))
		}
	}
	if cfg.ACPProvider == targetName {
		cfg.ACPProvider = replacement
		if replacement != "" {
			fmt.Printf(locale.T("ACP 使用的配置已重置，切换到 %q\n", "ACP selection reset. Switched to %q\n"), replacement)
		} else {
			fmt.Println(locale.T("ACP 使用的配置已清空。", "ACP selection cleared."))
		}
	}

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q\n", locale.T("已删除 Provider", "Successfully deleted provider"), targetName)
	return nil
}

func runProviderMove(sourceName, targetName string, force bool) error {
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
		prompt := locale.Tf("Provider %q 已存在，是否覆盖？(y/N): ", "Provider %q already exists. Overwrite? (y/N): ", target)
		if !confirmed(prompt, force) {
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
	if cfg.ACPProvider == source {
		cfg.ACPProvider = target
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
	if p.ModelOverrides != nil {
		cloned.ModelOverrides = make(map[string]string, len(p.ModelOverrides))
		for k, v := range p.ModelOverrides {
			cloned.ModelOverrides[k] = v
		}
	}
	return cloned
}

func init() {
	providerCmd.AddCommand(
		newProviderSetCommand("set [name]"),
		newProviderListCommand("ls"),
		newProviderUseCommand("use [--acp] [provider]"),
		newProviderCopyCommand("cp <source> <target>"),
		newProviderMoveCommand("mv <source> <target>"),
		newProviderRemoveCommand("rm <name>"),
		newProviderPreviewCommand("preview"),
		newMapCommand("map [provider-name]"),
		newModelsCommand("models"),
		newEnvCommand("env [KEY VALUE | ls | rm KEY | mv OLD NEW]"),
	)
	rootCmd.AddCommand(providerCmd, cpCmd, mvCmd, rmCmd)
}
