package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

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
	return &cobra.Command{
		Use:   "claude [alias]",
		Short: locale.T("登录 Claude 订阅账号", "Authenticate a Claude subscription account"),
		Long: locale.T(`使用 Claude Code 自身登录；每个账号作为一个 Provider，可用 ccl ls 和 ccl use 选择。

  ccl oauth claude work       登录账号 work
  ccl oauth claude personal   登录另一个账号
  ccl use work                切换到工作账号
  ccl --provider personal     仅本次使用另一个账号

不指定别名时优先使用账号邮箱命名；同名重新登录会保留该账号的设置和历史。`, `Sign in through Claude Code. Each account is a provider selected by ccl ls and ccl use.

  ccl oauth claude work       Sign in as work
  ccl oauth claude personal   Sign in to another account
  ccl use work                Switch to work
  ccl --provider personal     Use the other account once

Without an alias, the account email is used when available; re-login preserves the profile.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return loginClaudeAccount(cmd.Context(), cmd.OutOrStdout(), name)
		},
	}
}

func claudeAccountProvider(name, id string) provider.Provider {
	return provider.Provider{Name: name, Type: "anthropic", Endpoint: "oauth://claude", OAuthProvider: "claude", OAuthAccountCredential: id}
}

func loginClaudeAccount(ctx context.Context, out io.Writer, name string) error {
	name = strings.TrimSpace(name)
	if name != "" {
		if err := validateProviderAlias(name); err != nil {
			return err
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	existing, exists := cfg.Providers[name]
	if exists && !provider.IsClaudeAccount(existing) {
		return fmt.Errorf("provider %q already exists and is not a Claude account", name)
	}
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	var candidate claudeaccount.Candidate
	if exists {
		candidate, err = store.PrepareExisting(name, existing.OAuthAccountCredential)
	} else {
		candidate, err = store.PrepareNew(name)
	}
	if err != nil {
		return err
	}
	if !IsInstalled() {
		if err := AutoInstall(); err != nil {
			if candidate.New {
				_ = os.RemoveAll(candidate.Path)
			}
			return err
		}
	}
	fmt.Fprintf(out, locale.T("正在通过 Claude Code 登录账号 %q…\n", "Signing in to Claude account %q through Claude Code...\n"), candidate.Name)
	if err := runClaudeAccountLogin(ctx, candidate.Path); err != nil {
		if candidate.New {
			_ = os.RemoveAll(candidate.Path)
		}
		return fmt.Errorf("Claude account login failed: %w", err)
	}
	if name == "" {
		email := strings.TrimSpace(claudeAccountEmail(ctx, candidate.Path))
		if email != "" && validateProviderAlias("claude-"+email) == nil {
			candidate.Name = "claude-" + email
		}
	}
	var activated string
	err = config.Update(func(cfg *provider.Config) error {
		target := candidate.Name
		if name == "" {
			base := target
			for n := 2; ; n++ {
				if _, ok := cfg.Providers[target]; !ok {
					break
				}
				target = fmt.Sprintf("%s-%d", base, n)
			}
		} else if latest, ok := cfg.Providers[target]; ok && (!provider.IsClaudeAccount(latest) || latest.OAuthAccountCredential != candidate.ID()) {
			return fmt.Errorf("provider %q changed during login; account profile saved at %s", target, candidate.Path)
		}
		p := cfg.Providers[target]
		p.Name = target
		p.Type = "anthropic"
		p.Endpoint = "oauth://claude"
		p.APIKey = ""
		p.AnthropicAuth = ""
		p.OAuthProvider = "claude"
		p.OAuthAccountCredential = candidate.ID()
		cfg.Providers[target] = p
		cfg.ActiveProvider = target
		cfg.DefaultClaudeProvider = target
		activated = target
		return nil
	})
	if err != nil {
		return fmt.Errorf("Claude login succeeded, but the provider could not be saved; profile is at %s: %w", candidate.Path, err)
	}
	fmt.Fprintf(out, locale.T("已登录并切换到 Claude Provider %q。\n", "Authenticated Claude as provider %q and switched active provider.\n"), activated)
	return nil
}

// The preceding account-selector version was available on main. Import its
// local aliases once so already logged-in profiles do not require re-login.
func importLegacyClaudeAccounts(ctx context.Context) error {
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	return store.ImportLegacy(ctx, func(accounts map[string]string, active string) error {
		if len(accounts) == 0 {
			return nil
		}
		return config.Update(func(cfg *provider.Config) error {
			names := make([]string, 0, len(accounts))
			for name := range accounts {
				names = append(names, name)
			}
			sort.Strings(names)
			imported := map[string]string{}
			for _, name := range names {
				id := accounts[name]
				target := name
				if old, ok := cfg.Providers[target]; ok && (!provider.IsClaudeAccount(old) || old.OAuthAccountCredential != id) {
					base := name + "-claude"
					target = base
					for n := 2; ; n++ {
						existing, ok := cfg.Providers[target]
						if !ok || (provider.IsClaudeAccount(existing) && existing.OAuthAccountCredential == id) {
							break
						}
						target = fmt.Sprintf("%s-%d", base, n)
					}
				}
				if _, ok := cfg.Providers[target]; !ok {
					cfg.Providers[target] = claudeAccountProvider(target, id)
				}
				imported[name] = target
			}
			if target := imported[active]; target != "" && provider.IsDefaultProvider(cfg.ActiveProvider) {
				cfg.ActiveProvider = target
				cfg.DefaultClaudeProvider = target
			}
			return nil
		})
	})
}
