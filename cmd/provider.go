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
  use [name]     Switch the active provider
  cp/mv/rm       Copy, rename, delete
  map            Slot → model mapping (Opus/Sonnet/Haiku/Fable/Custom)
  models         Availability check for the model pool
  env            Provider-scoped environment variables
  preview        Show the Grok Build model overlay
  effort         Set the default reasoning effort for Grok Build

Root shortcuts: ccl set / ccl ls / ccl use / ccl map.
`,
}

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
		Short: "Switch providers; default uses Grok Build's own login",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runProviderUse("")
			}
			return runProviderUse(args[0])
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
			cmd.SilenceUsage = true
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
			cmd.SilenceUsage = true
			return runProviderMove(args[0], args[1], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Overwrite an existing target without confirmation")
	return cmd
}

func runProviderUse(name string) error {
	target := strings.TrimSpace(name)
	if target == "" {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		target, err = selectProviderToUse(cfg)
		if err != nil || target == "" {
			return err
		}
	}
	isDefault := provider.IsDefaultProvider(target)
	err := config.Update(func(c *provider.Config) error {
		if !isDefault {
			p, exists := c.Providers[target]
			if !exists {
				return fmt.Errorf(locale.T(
					"未找到 Provider %q。请先用 'ccl set' 添加，或用 'ccl ls' 检查拼写",
					"provider %q not found in configuration. Add it using 'ccl set' or check spelling with 'ccl ls'",
				), target)
			}
			if err := provider.ValidateRuntime(p); err != nil {
				return err
			}
		}
		if isDefault {
			c.LegacyClaudeDefault = ""
		}
		c.ActiveProvider = target
		return nil
	})
	if err != nil {
		return err
	}
	if isDefault {
		fmt.Println(locale.T("已切换到 default，使用 Grok Build 自身登录", "Switched to default using Grok Build's own login"))
	} else {
		fmt.Printf(locale.T("已切换到激活 Provider：%s\n", "Switched to active provider: %s\n"), target)
	}
	return nil
}

// selectProviderToUse runs the filter selector over the configured providers so
// `ccl use` without arguments can switch interactively, mirroring the selection
// list `ccl set` shows (but without the create-new entry — use only switches
// between existing providers). It returns "" when the user cancels.
func selectProviderToUse(cfg *provider.Config) (string, error) {
	names := make([]string, 0, len(cfg.Providers)+1)
	for name, p := range cfg.Providers {
		if provider.ValidateRuntime(p) == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%s", locale.T(
			"没有可用的 Provider。请用 'ccl set' 添加，或运行 ccl use default",
			"no providers available. Add one with 'ccl set' or run ccl use default",
		))
	}
	sort.Strings(names)
	names = append([]string{provider.DefaultProvider}, names...)
	selected := cfg.ActiveProvider
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
	if provider.IsDefaultProvider(target) {
		return fmt.Errorf(locale.T(
			"%q 是保留名（Grok Build 自身配置），请换一个名称",
			"%q is reserved for Grok Build's own configuration; choose another name",
		), target)
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
	clearedActive := false
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
		cfg = c
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	if clearedActive {
		fmt.Println(locale.T("当前 Provider 已清空，用 ccl use <name> 选择新的。", "Active provider cleared; choose one with ccl use <name>."))
	}

	fmt.Printf("✅ %s %q\n", locale.T("已删除 Provider", "Successfully deleted provider"), targetName)
	offerCredentialRemoval(cfg, removed, force, purge)
	return nil
}

// offerCredentialRemoval deletes a removed provider's OAuth credential when no
// remaining provider uses it — with --purge, or after an interactive yes. -y
// alone confirms the provider deletion only; credentials are kept.
func offerCredentialRemoval(cfg *provider.Config, removed provider.Provider, force, purge bool) {
	if provider.IsClaudeAccount(removed) {
		fmt.Println(locale.T("Claude 账号目录及会话历史已保留。", "Claude account profile and session history were kept."))
		return
	}
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
	if provider.IsDefaultProvider(target) {
		return fmt.Errorf(locale.T(
			"%q 是保留名（Grok Build 自身配置），请换一个名称",
			"%q is reserved for Grok Build's own configuration; choose another name",
		), target)
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
		newProviderUseCommand("use [provider]"),
		newProviderCopyCommand("cp <source> <target>"),
		newProviderMoveCommand("mv <source> <target>"),
		newProviderRemoveCommand("rm <name>"),
		newProviderPreviewCommand("preview"),
		newMapCommand("map [provider-name]"),
		newModelsCommand("models"),
		newEnvCommand("env [KEY VALUE | ls | rm KEY | mv OLD NEW]"),
		newProviderEffortCommand(),
	)
	rootCmd.AddCommand(providerCmd)
}
