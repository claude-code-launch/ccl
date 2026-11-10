package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/claude-code-launch/ccl/internal/browser"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/webui"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newServeCommand())
}

func newServeCommand() *cobra.Command {
	var providerName string
	var workspaces []string
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        locale.T("在本地浏览器中使用 Grok Build 会话", "Use Grok Build conversations in a local browser"),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if port < 0 || port > 65535 {
				return errors.New(locale.T("--port 必须介于 0 到 65535", "--port must be between 0 and 65535"))
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := providerName
			if name == "" {
				name = cfg.ActiveProvider
			}
			if provider.IsDefaultProvider(name) {
				return errors.New(locale.T("浏览器会话需要已配置的 Provider，请通过 --provider 指定名称",
					"browser sessions require a configured provider. Select one with --provider"))
			}
			p, err := resolveProviderFromConfig(cfg, name)
			if err != nil {
				return err
			}
			if err := provider.ValidateRuntime(p); err != nil {
				return err
			}
			grok, err := lookGrokBinary()
			if err != nil {
				return errors.New(locale.T("未找到 Grok Build CLI，请先安装 Grok Build", "Grok Build CLI not found. Install Grok Build first"))
			}
			configPath := config.ConfigPath()
			if configPath == "" {
				return errors.New(locale.T("无法确定 CCL 配置目录", "could not resolve the CCL config directory"))
			}
			if len(workspaces) == 0 {
				workspaces = []string{"."}
			}
			oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
			_, ownsLog, err := oauthproxy.EnsureSessionLog("grok_browser")
			if err != nil {
				return err
			}
			if ownsLog {
				defer oauthproxy.CloseLog()
			}
			ctx, stop := process.SignalContext(cmd.Context())
			defer stop()
			return webui.Serve(ctx, webui.Options{
				GrokPath: grok, StateDir: filepath.Join(filepath.Dir(configPath), "serve", "grok"),
				Provider: p, Workspaces: workspaces, Port: port,
			}, func(url string) {
				fmt.Fprintln(cmd.OutOrStdout(), url)
				if !noOpen {
					if err := browser.Open(url); err != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), locale.T("无法自动打开浏览器，请打开上面的链接。", "Could not open a browser. Open the link above."))
					}
				}
			})
		},
	}
	cmd.FParseErrWhitelist.UnknownFlags = false
	cmd.Flags().StringVarP(&providerName, "provider", "p", "", locale.T("已配置的 Provider（默认用当前 Provider）", "Configured provider (defaults to the active provider)"))
	cmd.Flags().StringArrayVar(&workspaces, "workspace", nil, locale.T("会话目录，可重复指定（默认为当前目录）", "Session workspace, repeatable (defaults to the current directory)"))
	cmd.Flags().IntVar(&port, "port", 0, locale.T("本地端口，0 表示自动分配", "Loopback port, 0 selects a free port"))
	cmd.Flags().BoolVar(&noOpen, "no-open", false, locale.T("仅输出链接", "Print the link without opening a browser"))
	return cmd
}
