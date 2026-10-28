package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// Claude Code performs its own OAuth and token refresh, including macOS
// Keychain integration. CCL never reads or copies the account's credentials.
var runClaudeAccountLogin = func(ctx context.Context, profile string) error {
	path, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("Claude Code is required for account login: %w", err)
	}
	child := process.Command(path, "auth", "login", "--claudeai")
	child.Env = claudeaccount.Environment(os.Environ(), profile)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = child.Process.Kill() })
	err = child.Wait()
	stop()
	return err
}

// On an unnamed login, ask Claude Code for a human-readable alias without
// reading its credentials. Older CLIs that lack JSON status keep the generated
// fallback name, and login is never failed by this optional lookup.
var claudeAccountEmail = func(ctx context.Context, profile string) string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	child := process.Command(path, "auth", "status", "--json")
	child.Env = claudeaccount.Environment(os.Environ(), profile)
	var output strings.Builder
	child.Stdout = &output
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		return ""
	}
	stop := context.AfterFunc(ctx, func() { _ = child.Process.Kill() })
	err = child.Wait()
	stop()
	if err != nil {
		return ""
	}
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Email      string `json:"email"`
	}
	if json.Unmarshal([]byte(output.String()), &status) != nil || !status.LoggedIn || status.AuthMethod != "claude.ai" {
		return ""
	}
	return status.Email
}

func newClaudeAccountCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "claude [name]",
		Short: locale.T("登录或重新登录 Claude 订阅账号", "Log in or re-authenticate a Claude subscription account"),
		Long: locale.T(`使用 Claude Code 自己的登录与令牌刷新，为每个账号保留独立配置。

  ccl oauth claude             首次登录；已有账号时选择或新增账号
  ccl oauth claude work        登录账号 work；再次运行可重新登录
  ccl oauth claude add         新增账号并自动生成名称
  ccl oauth claude personal    登录另一个账号
  ccl oauth claude list        查看账号
  ccl oauth claude use work    选择账号并切换到 default
  ccl oauth claude use default 恢复 Claude Code 原本的配置
  ccl --account personal       本次启动使用 personal，不修改默认选择

各账号的 Claude 设置和会话历史分别保存。
`, `Use Claude Code's own login and token refresh with a separate profile per account.

  ccl oauth claude             Sign in on first use; otherwise select or add an account
  ccl oauth claude work        Sign in as work; run again to re-authenticate
  ccl oauth claude add         Add an account with a generated name
  ccl oauth claude personal    Sign in to another account
  ccl oauth claude list        List accounts
  ccl oauth claude use work    Select an account and switch to default
  ccl oauth claude use default Restore Claude Code's original configuration
  ccl --account personal       Use personal for this launch without changing the default

Each account keeps separate Claude settings and history.
`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return chooseClaudeAccount(cmd.Context(), cmd.OutOrStdout())
			}
			return loginClaudeAccount(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	command.AddCommand(&cobra.Command{Use: "add [name]", Short: "Add or re-authenticate a Claude account", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return loginClaudeAccount(cmd.Context(), cmd.OutOrStdout(), name)
		}})
	command.AddCommand(&cobra.Command{Use: "list", Short: "List local Claude accounts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listClaudeAccounts(cmd.OutOrStdout()) }})
	command.AddCommand(&cobra.Command{Use: "use <name|default>", Short: "Select a Claude account", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return selectClaudeAccount(cmd.Context(), cmd.OutOrStdout(), args[0])
		}})
	return command
}

func chooseClaudeAccount(ctx context.Context, out io.Writer) error {
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	names, active, err := store.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return loginClaudeAccount(ctx, out, "")
	}
	if !term.IsTerminal(os.Stdin.Fd()) {
		if err := listClaudeAccounts(out); err != nil {
			return err
		}
		fmt.Fprintln(out, locale.T("用 ccl oauth claude use <name> 选择，或用 ccl oauth claude add [name] 登录新账号。", "Use ccl oauth claude use <name> to select, or ccl oauth claude add [name] to sign in."))
		return nil
	}
	choices := make([]string, 0, len(names)+2)
	values := []string{"default"}
	if active != "" {
		values = []string{active, "default"}
	}
	for _, name := range names {
		if name != active {
			values = append(values, name)
		}
	}
	for index, name := range values {
		selected := name == active || (name == "default" && active == "")
		marker := ""
		if selected {
			marker = locale.T(" (当前)", " (selected)")
		}
		choices = append(choices, fmt.Sprintf("%d. %s%s", index+1, name, marker))
	}
	add := fmt.Sprintf("%d. %s", len(choices)+1, locale.T("登录新账号", "Sign in to another account"))
	choices = append(choices, add)
	chosen, err := runSelect(locale.T("选择 Claude 账号：", "Select a Claude account:"), choices)
	if err != nil || chosen == "" {
		return err
	}
	if chosen == add {
		return loginClaudeAccount(ctx, out, "")
	}
	for index, name := range values {
		if chosen == choices[index] {
			return selectClaudeAccount(ctx, out, name)
		}
	}
	return fmt.Errorf("unknown Claude account selection")
}

func selectDefaultProvider() error {
	return config.Update(func(cfg *provider.Config) error {
		cfg.ActiveProvider = provider.DefaultProvider
		return nil
	})
}

func loginClaudeAccount(ctx context.Context, out io.Writer, name string) error {
	if !IsInstalled() {
		if err := AutoInstall(); err != nil {
			return err
		}
	}
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	candidate, err := store.Prepare(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, locale.T("正在通过 Claude Code 登录账号 %q…\n", "Signing in to Claude account %q through Claude Code...\n"), candidate.Name)
	if err := runClaudeAccountLogin(ctx, candidate.Path); err != nil {
		if candidate.New {
			_ = os.RemoveAll(candidate.Path)
		}
		return fmt.Errorf("Claude account login failed: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		candidate = store.PreferName(candidate, claudeAccountEmail(ctx, candidate.Path))
	}
	if err := store.Activate(ctx, candidate); err != nil {
		return fmt.Errorf("Claude login succeeded, but the account could not be saved; profile is at %s: %w", candidate.Path, err)
	}
	if err := selectDefaultProvider(); err != nil {
		return fmt.Errorf("account saved, but could not select default provider: %w", err)
	}
	fmt.Fprintf(out, locale.T("已登录并选择账号 %q。\n", "Signed in and selected Claude account %q.\n"), candidate.Name)
	return nil
}

func selectClaudeAccount(ctx context.Context, out io.Writer, name string) error {
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	if err := store.Select(ctx, name); err != nil {
		return err
	}
	if err := selectDefaultProvider(); err != nil {
		return fmt.Errorf("account selected, but could not select default provider: %w", err)
	}
	fmt.Fprintf(out, locale.T("已选择 Claude 账号 %q。\n", "Selected Claude account %q.\n"), name)
	return nil
}

func listClaudeAccounts(out io.Writer) error {
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	names, selected, err := store.List()
	if err != nil {
		return err
	}
	if selected == "" {
		fmt.Fprintln(out, "* default")
	} else {
		fmt.Fprintln(out, "  default")
	}
	for _, name := range names {
		marker := "  "
		if name == selected {
			marker = "* "
		}
		fmt.Fprintln(out, marker+name)
	}
	return nil
}
