package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/browser"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/webui"
	"github.com/spf13/cobra"
)

func newWebCommand() *cobra.Command {
	var (
		port           int
		host           string
		workspace      string
		token          string
		maxConcurrency int
		providerName   string
		model          string
		effort         string
		openBrowser    bool
		noOpen         bool
		shell          bool
	)
	cmd := &cobra.Command{
		Use:   "web [--port 8099] [--workspace .] [--host 127.0.0.1] [--token <token>]",
		Short: locale.T("在浏览器中使用 ccl 会话（HTTP + SSE）", "Use ccl conversations from a browser (HTTP + SSE)"),
		Long: locale.T(`在浏览器中运行 ccl 会话：一个对话就是一个 ccl 终端实例（读写工作目录、运行工具、切换 provider）。

每一个回合起一个新的 Claude Code 进程，用 --resume 续接同一会话，所以 provider、模型和 effort 在回合边界生效，
停止就是把进程杀掉。会话历史存放在 ~/.ccl/web/sessions/<id>/{meta.json,events.jsonl}，重启后仍在。

页面只做两件事：对话（左会话列表 + 右消息流）与设置抽屉（provider、endpoint、API key、模型槽位、effort）。
设置抽屉通过 /api/config 读写 ~/.ccl/config.yaml；正在运行的回合使用它启动时的配置快照，不受影响。

鉴权：绑定回环地址（默认 127.0.0.1）时不需要 token，本机打开就能用；--host 指定其它地址（如容器里的
0.0.0.0）时必须提供 --token 或 CCL_WEB_TOKEN，否则拒绝启动。所有 /api 请求要
Authorization: Bearer <token>；?token= 只用于 SSE 与事件读取（EventSource 无法设置请求头）。
工作目录锁死为 --workspace，客户端不能指定。
默认在就绪后打开系统浏览器；容器内用 --no-open。

示例：
  ccl web                                   # 本机，http://127.0.0.1:8099
  ccl web --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"`, `Run ccl conversations in a browser: one conversation is one ccl terminal session (it reads and writes the
workspace, runs tools, and switches provider).

Every turn starts a new Claude Code process and resumes the same session with --resume, so provider, model
and effort take effect at turn boundaries, and stopping a turn kills the process. History lives in
~/.ccl/web/sessions/<id>/{meta.json,events.jsonl} and survives restarts.

The page does two things: conversations (session list plus message stream) and a settings drawer (providers,
endpoint, API key, model slots, effort). The drawer reads and writes ~/.ccl/config.yaml through /api/config;
a running turn keeps the configuration snapshot it was started with.

Authentication: no token is needed while listening on a loopback address (the default, 127.0.0.1). Any other
--host, such as 0.0.0.0 in a container, requires --token or CCL_WEB_TOKEN and refuses to start without one.
Every /api request then needs Authorization: Bearer <token>; ?token= is accepted only by the SSE and events
endpoints, which EventSource cannot authenticate with a header. The workspace is fixed by --workspace.
The browser opens once the server is ready; use --no-open in a container.

Examples:
  ccl web                                   # local, http://127.0.0.1:8099
  ccl web --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"`),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CCL_WEB_TOKEN"))
			}
			// A loopback listener is reachable only from this machine, so it needs
			// no credential; anything else is exposed and must have one.
			requiresToken := !isLoopbackHost(host)
			if requiresToken && strings.TrimSpace(token) == "" {
				return fmt.Errorf("--host %s is not a loopback address, so a token is required: pass --token or set CCL_WEB_TOKEN", host)
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("--port must be between 1 and 65535, got %d", port)
			}
			abs, err := filepath.Abs(workspace)
			if err != nil {
				return fmt.Errorf("resolve workspace: %w", err)
			}
			server, err := webui.New(webui.Config{
				Token:           token,
				RequireToken:    requiresToken,
				Workspace:       abs,
				MaxConcurrency:  maxConcurrency,
				DefaultProvider: providerName,
				DefaultModel:    model,
				DefaultEffort:   effort,
				Version:         Version,
				SelfBinary:      selfBinary(),
				LatestVersion:   fetchLatestReleaseVersion,
				Shell:           shell,
			})
			if err != nil {
				return err
			}

			address := net.JoinHostPort(host, strconv.Itoa(port))
			listener, err := net.Listen("tcp", address)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", address, err)
			}
			httpServer := &http.Server{
				Handler:           server.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				// A turn streams for as long as Claude Code runs; there is no
				// write deadline that would not cut it off.
				IdleTimeout: 120 * time.Second,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpServer.Shutdown(shutdownCtx)
			}()

			displayHost := host
			if displayHost == "0.0.0.0" || displayHost == "::" || displayHost == "" {
				displayHost = "127.0.0.1"
			}
			url := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port)) + "/"
			fmt.Fprintf(os.Stderr, locale.T(
				"ccl web 已启动 · %s · workspace %s\n停止：Ctrl+C\n",
				"ccl web listening · %s · workspace %s\nStop with Ctrl+C\n",
			), url, abs)
			// --no-open wins: a headless container must be able to turn the
			// browser off regardless of --open.
			if openBrowser && !noOpen {
				// A headless container has no browser; that is not an error.
				if err := browser.Open(url); err != nil {
					fmt.Fprintf(os.Stderr, locale.T(
						"ccl: 无法打开浏览器（%v）；请手动打开 %s\n",
						"ccl: could not open a browser (%v); open %s yourself\n",
					), err, url)
				}
			}
			if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			<-shutdownDone
			return nil
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&port, "port", 8099, "TCP port to listen on")
	flags.StringVar(&host, "host", "127.0.0.1", "Address to bind (anything but a loopback address requires --token)")
	flags.BoolVar(&openBrowser, "open", true, "Open the system browser once the server is ready (--open=false to disable)")
	flags.BoolVar(&noOpen, "no-open", false, "Do not open a browser (containers); overrides --open")
	flags.StringVar(&workspace, "workspace", ".", "Directory every conversation runs in (clients cannot change it)")
	flags.StringVar(&token, "token", "", "Bearer token required by /api (default: CCL_WEB_TOKEN)")
	flags.IntVar(&maxConcurrency, "max-concurrency", 3, "Maximum simultaneous Claude Code processes")
	flags.StringVar(&providerName, "provider", "", "Provider new conversations start with (default: active provider)")
	flags.StringVar(&model, "model", "", "Model new conversations start with (slot or full id)")
	flags.StringVar(&effort, "effort", "", "Effort new conversations start with (low, medium, high, xhigh, max)")
	flags.BoolVar(&shell, "shell", false, "Offer a terminal (a shell in the workspace) in the page")
	return cmd
}

// isLoopbackHost reports whether host names only this machine. An empty host
// makes net.Listen bind every interface, so it is not treated as loopback.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func init() {
	rootCmd.AddCommand(newWebCommand())
}

// selfBinary is this ccl executable, which the page runs for `ccl oauth`.
func selfBinary() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
