package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var envCmd = newEnvCommand("env [KEY VALUE | ls | rm KEY | mv OLD NEW]")

// newEnvCommand manages a provider's extra environment variables.
func newEnvCommand(use string) *cobra.Command {
	// Per command, not package level: newEnvCommand is built twice, once for
	// `ccl env` and once for `ccl provider env`.
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Manage environment variables",
		Long: `Manage extra environment variables ccl passes to Claude Code for a provider
(the active one, or --provider NAME / --acp).

Set or modify a variable:
  ccl provider env KEY VALUE

List all variables:
  ccl provider env ls

Delete a variable:
  ccl provider env rm KEY
  ccl provider env rm KEY -y        # skip the confirmation

Rename a variable:
  ccl provider env mv OLD_KEY NEW_KEY
  ccl provider env mv OLD_KEY NEW_KEY -y   # overwrite an existing key

Settings ccl manages itself — slot models, the subagent model, context
sizing, and the connection — are not set here; the command names the one
to use. A variable the installed Claude Code does not read is saved with a
warning.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvSet(args, *target)
		},
	}
	addProviderTargetFlags(cmd, target)
	cmd.AddCommand(newEnvListCommand(target), newEnvRemoveCommand(target), newEnvMoveCommand(target))
	return cmd
}

// managedEnvKey reports whether ccl owns key through a provider field, and
// which command sets it. Writing it as a raw variable would silently compete
// with that field.
func managedEnvKey(key string) (string, bool) {
	switch strings.ToUpper(key) {
	case "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":
		return "ccl map --opus|--sonnet|--haiku|--fable|--custom <model>", true
	case claude.SubagentModelEnv:
		return "ccl map --subagent <model>", true
	case provider.EnvMaxContextTokens, provider.EnvAutoCompactWindow, provider.EnvAutoCompactPct:
		return "ccl set (Context & Compact)", true
	case "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN":
		return "ccl set (Connection)", true
	}
	return "", false
}

func runEnvSet(args []string, target providerTarget) error {
	if len(args) != 2 {
		return errors.New(locale.T("期望 KEY 和 VALUE 参数，或子命令（ls、rm、mv）。见 `ccl provider env --help`", "expected KEY and VALUE arguments, or a subcommand (ls, rm, mv). See ccl provider env --help"))
	}
	name, _, err := target.load()
	if err != nil {
		return err
	}

	key := strings.TrimSpace(args[0])
	val := strings.TrimSpace(args[1])
	if key == "" {
		return errors.New(locale.T("键不能为空", "key cannot be empty"))
	}
	if command, managed := managedEnvKey(key); managed {
		return fmt.Errorf(locale.T(
			"%s 由 ccl 管理，请用 %s 设置",
			"%s is managed by ccl; set it with %s",
		), key, command)
	}
	if err := updateProvider(name, func(p *provider.Provider) error {
		if p.Env == nil {
			p.Env = make(map[string]string)
		}
		p.Env[key] = val
		return nil
	}); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ %s=%s\n", key, val)
	if read, known := claudeReadsEnvVar(key); known && !read {
		fmt.Printf(locale.T(
			"⚠️ 已安装的 Claude Code 不读取 %s，设置后不会生效。\n",
			"⚠️ The installed Claude Code does not read %s; it will have no effect.\n",
		), key)
	}
	return nil
}

// newEnvListCommand lists environment variables.
func newEnvListCommand(target *providerTarget) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all environment variables",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvList(*target)
		},
	}
}

func runEnvList(target providerTarget) error {
	name, p, err := target.load()
	if err != nil {
		return err
	}
	if len(p.Env) == 0 {
		fmt.Printf(locale.T("%q 未配置环境变量。\n", "No environment variables configured for %q.\n"), name)
		return nil
	}

	var keys []string
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Printf(locale.T("%q 的环境变量：\n", "Environment variables for %q:\n"), name)
	for _, k := range keys {
		fmt.Printf("  %s=%s\n", k, p.Env[k])
	}
	return nil
}

// newEnvRemoveCommand deletes an environment variable.
func newEnvRemoveCommand(target *providerTarget) *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:   "rm KEY",
		Short: "Delete an environment variable",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvRemove(args[0], yes, *target)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without confirmation")
	return cmd
}

func runEnvRemove(arg string, force bool, target providerTarget) error {
	key := strings.TrimSpace(arg)
	if key == "" {
		return errors.New(locale.T("键不能为空", "key cannot be empty"))
	}
	name, p, err := target.load()
	if err != nil {
		return err
	}
	if _, exists := p.Env[key]; !exists {
		return fmt.Errorf(locale.T("键 %q 不存在于 %q", "key %q not found in %q"), key, name)
	}

	prompt := fmt.Sprintf(locale.T("确定要删除 %s 吗？(y/N): ", "Delete %s? (y/N): "), key)
	if !confirmed(prompt, force) {
		fmt.Println(locale.T("已取消。", "Cancelled."))
		return nil
	}

	if err := updateProvider(name, func(p *provider.Provider) error {
		delete(p.Env, key)
		if len(p.Env) == 0 {
			p.Env = nil
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf(locale.T("✅ 已删除 %s\n", "✅ Deleted %s\n"), key)
	return nil
}

// newEnvMoveCommand renames an environment variable.
func newEnvMoveCommand(target *providerTarget) *cobra.Command {
	yes := false
	cmd := &cobra.Command{
		Use:   "mv OLD_KEY NEW_KEY",
		Short: "Rename an environment variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvMove(args[0], args[1], yes, *target)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Overwrite an existing key without confirmation")
	return cmd
}

func runEnvMove(oldArg, newArg string, force bool, target providerTarget) error {
	oldKey := strings.TrimSpace(oldArg)
	newKey := strings.TrimSpace(newArg)
	if oldKey == "" || newKey == "" {
		return errors.New(locale.T("键不能为空", "keys cannot be empty"))
	}
	if oldKey == newKey {
		fmt.Println(locale.T("新旧键相同，无需操作。", "Old and new keys are the same, nothing to do."))
		return nil
	}
	if command, managed := managedEnvKey(newKey); managed {
		return fmt.Errorf(locale.T(
			"%s 由 ccl 管理，请用 %s 设置",
			"%s is managed by ccl; set it with %s",
		), newKey, command)
	}

	name, p, err := target.load()
	if err != nil {
		return err
	}
	val, exists := p.Env[oldKey]
	if !exists {
		return fmt.Errorf(locale.T("键 %q 不存在于 %q", "key %q not found in %q"), oldKey, name)
	}

	if _, exists := p.Env[newKey]; exists {
		prompt := fmt.Sprintf(locale.T("键 %s 已存在，是否覆盖？(y/N): ", "Key %s already exists. Overwrite? (y/N): "), newKey)
		if !confirmed(prompt, force) {
			fmt.Println(locale.T("已取消。", "Cancelled."))
			return nil
		}
	}

	if err := updateProvider(name, func(p *provider.Provider) error {
		if p.Env == nil {
			p.Env = make(map[string]string)
		}
		delete(p.Env, oldKey)
		p.Env[newKey] = val
		return nil
	}); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✅ Renamed %s → %s\n", oldKey, newKey)
	return nil
}

func init() {
	rootCmd.AddCommand(deprecatedRootAlias(envCmd, "ccl provider env"))
}
