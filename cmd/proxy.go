package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
	"github.com/spf13/cobra"
)

func proxyProvider(name string) (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, err
	}
	if name == "" {
		name = cfg.ActiveProvider
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return provider.Provider{}, fmt.Errorf("provider %q not found; use ccl ls or ccl set", name)
	}
	p.Name = name
	return p, nil
}

func runProxyStart(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("provider")
	if len(args) > 0 {
		if name != "" {
			return fmt.Errorf("specify the provider either by name or with --provider, not both")
		}
		name = args[0]
	}
	p, err := proxyProvider(strings.TrimSpace(name))
	if err != nil {
		return err
	}
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	settingsPath, err := filepath.Abs(claude.UserSettingsPath())
	if err != nil {
		return err
	}
	signalCtx, cancelSignals := signal.NotifyContext(cmd.Context(), proxySignals...)
	defer cancelSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 2*time.Minute)
	defer cancel()
	s, err := m.Start(ctx, executable, p, settingsPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), locale.T("代理已开启 · %s · %s\n直接运行 claude 即可。停止：ccl proxy stop\n", "Proxy running · %s · %s\nRun claude directly. Stop with: ccl proxy stop\n"), s.Provider, s.BaseURL)
	return nil
}

func newProxyCommand() *cobra.Command {
	command := &cobra.Command{Use: "proxy [provider]", Short: "Run a background provider proxy for direct Claude Code use", Args: cobra.MaximumNArgs(1), RunE: runProxyStart,
		Long: `Start a background CCL proxy and connect direct claude invocations to it.

  ccl proxy -p provider1        Start with a named provider
  ccl proxy start provider1     Equivalent explicit command
  ccl proxy                    Start with the active provider
  ccl proxy status             Show the running provider and local endpoint
  ccl proxy stop               Stop and restore previous Claude settings

The proxy uses the current user's Claude settings (or CLAUDE_CONFIG_DIR).
Provider changes require another start; starting a different provider switches
the proxy. Existing Claude sessions should be restarted after switching.
User edits to unrelated settings are retained when the proxy stops.`}
	command.PersistentFlags().StringP("provider", "p", "", "Provider name (defaults to the active provider)")
	start := &cobra.Command{Use: "start [provider]", Args: cobra.MaximumNArgs(1), Short: "Start or switch the background proxy", RunE: runProxyStart}
	stop := &cobra.Command{Use: "stop", Args: cobra.NoArgs, Short: "Stop the proxy and restore Claude settings", RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if err := m.Stop(ctx); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), locale.T("代理已停止，原有 Claude 配置已恢复", "Proxy stopped; previous Claude settings restored"))
		return nil
	}}
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Show proxy status without revealing credentials", RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		s, err := m.Read()
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("代理未开启", "Proxy is stopped"))
			return nil
		}
		if !m.Running(cmd.Context(), s) {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("代理未响应；运行 ccl proxy stop 恢复 Claude 配置", "Proxy is not responding; run ccl proxy stop to restore Claude settings"))
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\nEndpoint: %s\nPID: %d\nClaude settings: %s\n", s.Provider, s.BaseURL, s.PID, s.SettingsPath)
		return nil
	}}
	serve := &cobra.Command{Use: "serve", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, _ := cmd.Flags().GetString("provider")
		id, _ := cmd.Flags().GetString("instance")
		path, _ := cmd.Flags().GetString("settings-path")
		if id == "" || path == "" {
			return fmt.Errorf("internal proxy serve requires an instance and settings path; use ccl proxy start")
		}
		p, err := proxyProvider(name)
		if err != nil {
			return err
		}
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), proxySignals...)
		defer cancel()
		return m.Run(ctx, p, path, id)
	}}
	serve.Flags().String("instance", "", "Internal startup identity")
	serve.Flags().String("settings-path", "", "Internal Claude settings path")
	command.AddCommand(start, stop, status, serve)
	return command
}

func init() { rootCmd.AddCommand(newProxyCommand()) }
