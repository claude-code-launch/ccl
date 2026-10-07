package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
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
  map            Slot → model mapping (Opus/Sonnet/Haiku/Fable/Custom)
  models         Availability check for the model pool
  env            Provider-scoped environment variables
  preview        Show settings JSON injected into Claude Code
  on/off         Load the active provider, or run Claude Code with its own
                 configuration (claude.ai subscription)

Root shortcuts: ccl set / ccl ls / ccl use / ccl map.
`,
}

var cpCmd = newProviderCopyCommand("cp <source> <target>")
var mvCmd = newProviderMoveCommand("mv <source> <target>")

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
same provider entry is pinned for ccl acp; no provider settings are copied or
maintained separately. Until pinned, ACP follows the normal-mode provider;
--acp --follow removes the pin again.

Examples:
  ccl use cc
  ccl use --acp cc
  ccl use --acp --follow`,
		// Parse the tiny grammar without retaining mutable flag state between
		// invocations of the reusable Cobra root used by embedders and tests.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, forACP, follow, showHelp, err := parseProviderUseArgs(args)
			if err != nil {
				return err
			}
			if showHelp {
				return cmd.Help()
			}
			if follow {
				return runACPFollow()
			}
			return runProviderUse(name, forACP)
		},
	}
	// Register the flag for help and completion metadata. Parsing is handled
	// above so repeated in-process command execution does not retain its value.
	cmd.Flags().Bool("acp", false, "Switch the provider used by ACP mode")
	cmd.Flags().Bool("follow", false, "With --acp: stop pinning ACP and follow the normal-mode provider")
	return cmd
}

func parseProviderUseArgs(args []string) (name string, forACP, follow, showHelp bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--acp":
			forACP = true
		case "--follow":
			follow = true
		case "-h", "--help":
			showHelp = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, false, false, fmt.Errorf("unknown flag: %s", arg)
			}
			if name != "" {
				return "", false, false, false, fmt.Errorf("accepts at most 1 provider name, received %q and %q", name, arg)
			}
			name = arg
		}
	}
	if follow && (!forACP || name != "") {
		return "", false, false, false, fmt.Errorf("--follow is used as: ccl use --acp --follow")
	}
	return name, forACP, follow, showHelp, nil
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
	yes, purge := false, false
	cmd := &cobra.Command{
		Use:   use,
		Short: "Delete a provider configuration",
		Long: `Delete a provider configuration.

Deleting the provider a mode is using clears that selection; pick another
with ccl use. If the provider's OAuth credential is used by no other provider,
ccl offers to delete it too (--purge does so without asking).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderRemove(args[0], yes, purge)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without confirmation")
	cmd.Flags().BoolVar(&purge, "purge", false, "Also delete the provider's OAuth credential when no other provider uses it")
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

	reenabled := false
	err = config.Update(func(c *provider.Config) error {
		if _, exists := c.Providers[target]; !exists {
			return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), target)
		}
		if forACP {
			c.ACPProvider = target
			return nil
		}
		c.ActiveProvider = target
		// Choosing a provider means wanting it: turn provider loading back on.
		reenabled = c.ProviderOff
		c.ProviderOff = false
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if forACP {
		fmt.Printf(locale.T("ACP 已切换为使用 Provider 配置：%s\n", "ACP now uses provider configuration: %s\n"), target)
	} else {
		fmt.Printf(locale.T("已切换到激活 Provider：%s\n", "Switched to active provider: %s\n"), target)
		if reenabled {
			fmt.Println(locale.T("Provider 加载已重新打开（之前为 off）", "Provider loading is back on (it was off)"))
		}
	}
	return nil
}

// runACPFollow removes the ACP pin so ACP follows the normal-mode provider.
func runACPFollow() error {
	var active string
	if err := config.Update(func(c *provider.Config) error {
		c.ACPProvider = ""
		active = c.ActiveProvider
		return nil
	}); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	if active != "" {
		fmt.Printf(locale.T("ACP 现在跟随普通模式的 Provider（当前：%s）\n", "ACP now follows the normal-mode provider (currently %s)\n"), active)
	} else {
		fmt.Println(locale.T("ACP 现在跟随普通模式的 Provider。", "ACP now follows the normal-mode provider."))
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
		selected = cfg.EffectiveACPProvider()
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
		return fmt.Errorf("%s", locale.T("请提供源和目标名称，例如: ccl provider cp <source> <target>", "please provide both source and target names, e.g.: ccl provider cp <source> <target>"))
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

	copied := srcProvider.Clone()
	copied.Name = target
	if err := replaceProvider(target, copied); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q → %q\n", locale.T("已复制 Provider", "Successfully copied provider"), source, target)
	return nil
}

func runProviderRemove(name string, force, purge bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	targetName := strings.TrimSpace(name)
	if targetName == "" {
		return fmt.Errorf("%s", locale.T("请指定要删除的 Provider 名称，例如: ccl provider rm <name>", "please specify the provider name to delete, e.g.: ccl provider rm <name>"))
	}

	if _, exists := cfg.Providers[targetName]; !exists {
		return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found in configuration"), targetName)
	}

	prompt := locale.Tf("确定要删除 Provider %q？(y/N): ", "Are you sure you want to delete provider %q? (y/N): ", targetName)
	if !confirmed(prompt, force) {
		fmt.Println(locale.T("已取消删除。", "Deletion cancelled."))
		return nil
	}

	var removed provider.Provider
	clearedActive, clearedACP := false, false
	// Never switch to another provider on the user's behalf: that could quietly
	// move the next session onto a different (billed) source. Clear the
	// selection and say how to pick one.
	err = config.Update(func(c *provider.Config) error {
		removed = c.Providers[targetName]
		delete(c.Providers, targetName)
		if c.ActiveProvider == targetName {
			c.ActiveProvider = ""
			clearedActive = true
		}
		if c.ACPProvider == targetName {
			c.ACPProvider = ""
			clearedACP = true
		}
		cfg = c
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	if clearedActive {
		fmt.Println(locale.T("当前 Provider 已清空，用 ccl use <name> 选择新的。", "Active provider cleared; choose one with ccl use <name>."))
	}
	if clearedACP {
		fmt.Println(locale.T("ACP 的固定选择已清除，现在跟随普通模式的 Provider。", "ACP's pinned provider was cleared; ACP now follows the normal-mode provider."))
	}

	fmt.Printf("✅ %s %q\n", locale.T("已删除 Provider", "Successfully deleted provider"), targetName)
	offerCredentialRemoval(cfg, removed, force, purge)
	return nil
}

// offerCredentialRemoval deletes a removed provider's OAuth credential when no
// remaining provider uses it — with --purge, or after an interactive yes. -y
// alone confirms the provider deletion only; credentials are kept.
func offerCredentialRemoval(cfg *provider.Config, removed provider.Provider, force, purge bool) {
	name := filepath.Base(strings.TrimSpace(removed.OAuthAccountCredential))
	if name == "" || name == "." || credentialUsers(cfg)[name] > 0 {
		return
	}
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		return
	}
	if _, err := os.Lstat(filepath.Join(authDir, name)); err != nil {
		return
	}
	if !purge {
		if force {
			fmt.Printf(locale.T("凭据 %s 已无 Provider 使用，已保留（ccl oauth prune 可清理）\n", "Credential %s is no longer used and was kept (ccl oauth prune removes it)\n"), name)
			return
		}
		if !confirmed(locale.Tf("同时删除不再使用的凭据 %s？(y/N): ", "Also delete the now-unused credential %s? (y/N): ", name), false) {
			return
		}
	}
	if err := removeCredentialFile(authDir, name); err != nil {
		fmt.Printf(locale.T("删除凭据 %s 失败：%v\n", "Failed to delete credential %s: %v\n"), name, err)
		return
	}
	fmt.Printf(locale.T("✅ 已删除凭据 %s\n", "✅ Deleted credential %s\n"), name)
}

func runProviderMove(sourceName, targetName string, force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	source := strings.TrimSpace(sourceName)
	target := strings.TrimSpace(targetName)

	if source == "" || target == "" {
		return fmt.Errorf("%s", locale.T("请提供旧名称和新名称，例如: ccl provider mv <source> <target>", "please provide both source and target names, e.g.: ccl provider mv <source> <target>"))
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
	}

	err = config.Update(func(c *provider.Config) error {
		src, ok := c.Providers[source]
		if !ok {
			return fmt.Errorf(locale.T("未找到 Provider %q", "provider %q not found"), source)
		}
		moved := src.Clone()
		moved.Name = target
		c.Providers[target] = moved
		delete(c.Providers, source)
		if c.ActiveProvider == source {
			c.ActiveProvider = target
		}
		if c.ACPProvider == source {
			c.ACPProvider = target
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s %q → %q\n", locale.T("已重命名 Provider", "Successfully renamed provider"), source, target)
	return nil
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
		newProviderToggleCommand("on"),
		newProviderToggleCommand("off"),
	)
	// Root shortcuts for the less frequent provider commands are deprecated;
	// `rm` is gone from the root because `claude rm` is a Claude Code command.
	rootCmd.AddCommand(providerCmd,
		deprecatedRootAlias(cpCmd, "ccl provider cp"),
		deprecatedRootAlias(mvCmd, "ccl provider mv"))
}
