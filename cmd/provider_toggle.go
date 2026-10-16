package cmd

import (
	"context"
	"fmt"
	"github.com/claude-code-launch/ccl/internal/claude"
	"io"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// newProviderToggleCommand builds `ccl provider on|off`. Off makes plain
// launches run Claude Code with its own configuration — the claude.ai login and
// ~/.claude/settings.json — instead of loading the active provider.
func newProviderToggleCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Turn provider loading on or off for ccl launches",
		Long: `Turn provider loading on or off for normal ccl launches.

  ccl provider off     ccl starts Claude Code with its own configuration
                       (your claude.ai subscription), loading no provider
  ccl provider on      ccl loads the active provider again

A fresh install starts with loading off; activating a provider (ccl oauth,
Save & Activate in ccl set, ccl use) turns it on. ccl ls shows the current state.

While off, the environment is passed to Claude Code unchanged and no ccl
settings, proxy, or status line are added. ccl provider off fails whenever
ccl proxy is running; stop it first with ccl proxy stop. While off, ccl proxy
needs an explicit provider name, and plain ccl launches bypass that proxy's
settings overlay without stopping it. ccl bypass still applies. ccl acp
keeps using its acp_provider. Selecting a provider with ccl use turns provider
loading back on.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProviderToggle(cmd.OutOrStdout(), cmd.Name())
		},
	}
}

func runProviderToggle(out io.Writer, action string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	if action != "on" && action != "off" {
		return fmt.Errorf("expected on or off, got %q", action)
	}

	off := action == "off"
	// Off exists to run Claude Code on its own subscription. A running proxy
	// keeps rewriting Claude's settings for a provider, so refuse every off
	// while one runs (even a repeated off) and say how to stop it.
	if off {
		if name, running := runningProxyProvider(context.Background()); running {
			return fmt.Errorf(locale.T(
				"ccl proxy 正在为 %s 运行；请先运行 ccl proxy stop，再关闭 provider 加载",
				"ccl proxy is running for %s; stop it with ccl proxy stop before turning provider loading off",
			), name)
		}
	}
	if err := config.Update(func(c *provider.Config) error {
		c.ProviderOff = off
		return nil
	}); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}
	printProviderLoadingState(out, off, cfg.ActiveProvider)
	return nil
}

func printProviderLoadingState(out io.Writer, off bool, active string) {
	if off {
		fmt.Fprintln(out, locale.T(
			"Provider 加载：off —— ccl 将以 Claude Code 自身配置启动（claude.ai 订阅）",
			"Provider loading: off — ccl starts Claude Code with its own configuration (claude.ai subscription)",
		))
		return
	}
	if active == "" {
		fmt.Fprintln(out, locale.T(
			"Provider 加载：on（尚未选择激活 Provider）",
			"Provider loading: on (no active provider selected)",
		))
		return
	}
	fmt.Fprintf(out, locale.T(
		"Provider 加载：on —— ccl 使用激活 Provider：%s\n",
		"Provider loading: on — ccl uses the active provider: %s\n",
	), active)
}

// runClaudeWithoutProvider starts Claude Code exactly as the user's own
// configuration would, apart from ccl's launcher flags (bypass).
func runClaudeWithoutProvider(args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH: %w", err)
	}
	settingsPath, cleanup, err := claude.PlainSettingsWithoutProxy()
	if err != nil {
		return err
	}
	defer cleanup()
	if settingsPath != "" {
		args = append([]string{"--settings", settingsPath}, args...)
	}
	fmt.Fprintln(os.Stderr, locale.T(
		"ccl provider 已关闭：以 Claude Code 自身配置启动（ccl provider on 可恢复）",
		"ccl provider is off: starting Claude Code with its own configuration (ccl provider on to restore)",
	))
	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
