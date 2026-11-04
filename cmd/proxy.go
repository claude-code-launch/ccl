package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
	"github.com/spf13/cobra"
)

// recoverStaleProxy restores a dead Codex proxy's configuration, including
// Claude settings left behind by older shared-proxy versions.
// It is quiet unless it restored something, and never fatal.
func recoverStaleProxy() {
	m, err := proxymanager.Default()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restored, err := m.RecoverStale(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, locale.T("ccl: 无法恢复已退出代理的客户端配置：%v；请运行 ccl codex stop\n", "ccl: could not restore client settings for an exited proxy: %v; run ccl codex stop\n"), err)
		return
	}
	if restored {
		fmt.Fprintln(os.Stderr, locale.T("ccl: 代理已意外退出，原有客户端配置已恢复", "ccl: the proxy had exited; previous client settings restored"))
	}
}

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

// runningProxyProvider reports the provider a live standalone proxy serves. A
// stale journal of a dead proxy does not count: startup recovery restores it.
var runningProxyProvider = func(ctx context.Context) (string, bool) {
	m, err := proxymanager.Default()
	if err != nil {
		return "", false
	}
	s, err := m.Read()
	if err != nil || s == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.Provider, m.Running(ctx, s)
}

func runProxyStart(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("provider")
	if len(args) > 0 {
		if name != "" {
			return fmt.Errorf("specify the provider either by name or with --provider, not both")
		}
		name = args[0]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name = cfg.ActiveProvider
	}
	if provider.IsDefaultProvider(name) {
		return runProxyDefault(cmd)
	}
	p, err := proxyProvider(name)
	if err != nil {
		return err
	}
	if provider.IsClaudeAccount(p) {
		return fmt.Errorf("Claude subscription accounts use Claude Code directly; use ccl --provider %s", name)
	}
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	independent, _ := cmd.Flags().GetBool("independent")
	shared, _ := cmd.Flags().GetBool("shared")
	if independent && shared {
		return fmt.Errorf("choose --independent or --shared")
	}
	if independent {
		m.CodexMode = "independent"
	} else if shared {
		m.CodexMode = "shared"
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	signalCtx, cancelSignals := signal.NotifyContext(cmd.Context(), proxySignals...)
	defer cancelSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 2*time.Minute)
	defer cancel()
	s, err := m.Start(ctx, executable, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), locale.T("Codex 模型接入已开启 · %s · %s\n停止：ccl codex stop\n", "Codex model integration running · %s · %s\nStop with: ccl codex stop\n"), s.Provider, s.BaseURL)
	if s.Codex != nil {
		fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型目录已接入；重启 Codex 后可选择 CCL 模型。", "Codex model catalog connected; restart Codex to select CCL models."))
		if s.CodexMode == "independent" {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("独立提供商模式：不依赖 OpenAI 登录。恢复共享目录：ccl codex start --shared", "Independent provider mode: OpenAI login is not required. Restore the shared catalog: ccl codex start --shared"))
		}
	}
	return nil
}

// runProxyDefault turns off the Codex proxy and restores the original route.
func runProxyDefault(cmd *cobra.Command) error {
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	s, err := m.Read()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(cmd.OutOrStdout(), locale.T(
			"default 无需 CCL 模型接入；Codex 使用原有配置",
			"default needs no CCL integration; Codex uses its original configuration",
		))
		return nil
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), locale.T(
		"已停止 %s 的 Codex 模型接入，Codex 恢复原有配置\n",
		"Disconnected %s from Codex; Codex is back on its original configuration\n",
	), s.Provider)
	return nil
}

func newCodexCommand() *cobra.Command {
	command := &cobra.Command{Use: "codex [provider]", Short: "Connect provider models to Codex", Args: cobra.MaximumNArgs(1), RunE: runProxyStart,
		Long: `Connect a CCL provider's models to Codex.

  ccl codex -p provider1        Start with a named provider
  ccl codex start provider1     Equivalent explicit command
  ccl codex start --independent Use CCL without OpenAI account authentication
  ccl codex start --shared      Restore native OpenAI + CCL shared picker
  ccl codex                    Start with the active provider
  ccl codex start default      Disconnect CCL and restore Codex's original route
  ccl codex status             Show the running provider and local endpoint
  ccl codex stop               Stop and restore previous Codex settings

Open Codex once to create its profile (CODEX_HOME is supported). The proxy adds
the selected provider's models to Codex's picker. Native OpenAI models keep their
own route and credentials. Restart Codex after starting, switching or stopping
the proxy to reload the catalog. macOS shows a menu bar icon.
Independent mode exposes only CCL models through a custom provider. Chats resume
with their saved model provider: restore --independent for ccl_proxy chats, or
fork a chat to continue its history on a different provider. Use --shared to
return to native models. Starts and menu switches inherit the running mode;
after stopping the proxy, a start without either mode flag defaults to shared.
Only Codex configuration is managed; ordinary ccl/Claude Code launches remain
independent. Stopping restores only unchanged owned fields, retaining user edits.
Upgrading an older shared proxy also restores its legacy Claude settings once.`}
	command.PersistentFlags().StringP("provider", "p", "", "Provider name (defaults to the active provider)")
	command.PersistentFlags().Bool("independent", false, "Use a separate Codex provider without OpenAI account authentication")
	command.PersistentFlags().Bool("shared", false, "Use the native OpenAI provider and merged model catalog")
	command.MarkFlagsMutuallyExclusive("independent", "shared")
	start := &cobra.Command{Use: "start [provider]", Args: cobra.MaximumNArgs(1), Short: "Start or switch Codex model integration", RunE: runProxyStart}
	stop := &cobra.Command{Use: "stop", Args: cobra.NoArgs, Short: "Disconnect CCL models and restore Codex settings", RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if err := m.Stop(ctx); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入已停止，原有配置已恢复", "Codex model integration stopped; previous settings restored"))
		return nil
	}}
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Show Codex model integration status without revealing credentials", RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		s, err := m.Read()
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入未开启", "Codex model integration is stopped"))
			return nil
		}
		if !m.Running(cmd.Context(), s) {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入未响应；运行 ccl codex stop 恢复配置", "Codex model integration is not responding; run ccl codex stop to restore settings"))
			return nil
		}
		endpoint := s.BaseURL
		if s.Codex != nil {
			endpoint = s.Codex.BaseURL
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\nCodex mode: %s\nCodex endpoint: %s\nPID: %d\n", s.Provider, s.CodexMode, endpoint, s.PID)
		if s.Codex != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Codex config: %s\n", s.Codex.Path)
		}
		return nil
	}}
	serve := &cobra.Command{Use: "serve", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, _ := cmd.Flags().GetString("provider")
		id, _ := cmd.Flags().GetString("instance")
		if id == "" {
			return fmt.Errorf("internal proxy serve requires an instance; use ccl codex start")
		}
		p, err := proxyProvider(name)
		if err != nil {
			return err
		}
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		m.CodexListenAddress, _ = cmd.Flags().GetString("codex-listen-address")
		m.CodexMode, _ = cmd.Flags().GetString("codex-mode")
		ctx, cancel := signal.NotifyContext(cmd.Context(), proxySignals...)
		defer cancel()
		return m.Run(ctx, p, id)
	}}
	serve.Flags().String("instance", "", "Internal startup identity")
	serve.Flags().String("codex-mode", "shared", "Internal Codex provider mode")
	serve.Flags().String("codex-listen-address", "", "Internal replacement Codex listener address")
	// Proxy failures are runtime errors, not usage mistakes: print the error
	// once (from Execute) without cobra's usage block.
	for _, c := range []*cobra.Command{command, start, stop, status, serve} {
		c.SilenceUsage, c.SilenceErrors = true, true
	}
	menuBar := &cobra.Command{Use: "menubar", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		id, _ := cmd.Flags().GetString("instance")
		if id == "" {
			return fmt.Errorf("internal menu bar requires a proxy instance")
		}
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), proxySignals...)
		defer cancel()
		return m.RunMenuBar(ctx, id)
	}}
	menuBar.Flags().String("instance", "", "Internal proxy identity")
	command.AddCommand(start, stop, status, serve, menuBar)
	addProxyMenuCommands(command)
	return command
}

func init() { rootCmd.AddCommand(newCodexCommand()) }
