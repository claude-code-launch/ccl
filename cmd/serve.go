package cmd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/claude-code-launch/ccl/internal/browser"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/webui"
	"github.com/spf13/cobra"
)

func newServeCommand() *cobra.Command {
	var (
		port           int
		workspaces     []string
		workspaceRoots []string
		token          string
		maxConcurrency int
		providerName   string
		model          string
		effort         string
		openBrowser    bool
		noOpen         bool
		shell          bool
		noShell        bool
		lockWorkspaces bool
		socket         string
		basePath       string
	)
	cmd := &cobra.Command{
		Use:   "serve [--port <port> | --socket [path]] [--workspace dir]... [--workspace-root dir]... [--token <token>]",
		Short: locale.T("在本地浏览器中使用 Grok Build 会话（HTTP + SSE，或本机 Unix socket）", "Use Grok Build conversations from a local browser (HTTP + SSE, or a local Unix socket)"),
		Long: locale.T(`在本地浏览器中运行 ccl 会话：每个对话在项目目录中通过 ACP v1 启动 Grok Build（读写项目文件、运行工具、切换 Provider）。

每一个回合起一个新的 Grok Build ACP 进程，并用 LoadSession 续接同一会话，因此 Provider、模型、推理强度（effort）和权限模式
在回合边界生效；停止当前回合会立即取消 ACP 请求并回收进程。会话历史存放在 ~/.ccl/serve/sessions/<id>/，重启后仍在。

页面包含：会话列表、消息流（文件 diff、任务清单、交互式权限确认卡片）、项目文件编辑器与 Git 面板、Git Worktree 隔离分支、
网页终端，以及设置弹窗（Provider 管理、OAuth 订阅登录、Grok MCP 连接器、~/.grok/AGENTS.md 指令、ccl cloud 同步）。

服务严格绑定在回环地址 127.0.0.1（或 0600 权限的本机 Unix socket --socket）。TCP 模式默认自动生成一次性会话 token 并通过
启动链接的 #fragment 传给浏览器（也可通过 --token 或 CCL_SERVE_TOKEN 指定固定 token），同时校验回环 Host 与同机 Origin。`, `Run ccl conversations in a local browser: each conversation drives Grok Build over ACP v1 in its project directory
(reading and editing files, running tools, and switching providers).

Every turn starts a fresh Grok Build ACP worker and resumes the conversation with LoadSession, so provider, model,
reasoning effort, and permission mode take effect at turn boundaries. Session history lives under
~/.ccl/serve/sessions/<id>/ and survives restarts.

The workspace UI includes a session list, streaming thread (inline diffs, plan checklists, interactive permission cards),
file editor, Git panel, isolated Git worktrees, optional web terminal (--no-shell to disable), and settings (providers,
OAuth subscription logins, Grok MCP connectors, ~/.grok/AGENTS.md instructions, and ccl cloud sync).

TCP always binds 127.0.0.1 and checks loopback Host and Origin headers. When --token / CCL_SERVE_TOKEN is omitted, a
per-server random token is generated and passed in the launch URL fragment (#token). --socket serves the same API over a
mode-0600 local Unix socket without opening a port.`),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := socketPath(cmd, socket, args)
			if err != nil {
				return err
			}
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port must be between 0 and 65535, got %d", port)
			}
			if len(workspaces) == 0 && len(workspaceRoots) == 0 {
				workspaces = []string{"."}
			}
			if basePath, err = webui.NormalizeBasePath(strings.TrimSpace(basePath)); err != nil {
				return fmt.Errorf("--base-path: %w", err)
			}

			if loaded, loadErr := config.Load(); loadErr == nil {
				oauthproxy.ConfigureLogLevel(configuredLogLevel(loaded.LogLevel))
				if _, ownsLog, logErr := oauthproxy.EnsureSessionLog("grok_serve"); logErr == nil && ownsLog {
					defer oauthproxy.CloseLog()
				}
			}

			if token == "" {
				token = strings.TrimSpace(os.Getenv("CCL_SERVE_TOKEN"))
			}
			generatedToken := false
			if socket == "" && token == "" {
				token = rand.Text()
				generatedToken = true
			}

			server, err := webui.New(webui.Config{
				Token:           token,
				RequireToken:    socket == "",
				Workspaces:      workspaces,
				WorkspaceRoots:  workspaceRoots,
				MaxConcurrency:  maxConcurrency,
				DefaultProvider: providerName,
				DefaultModel:    model,
				DefaultEffort:   effort,
				Version:         Version,
				SelfBinary:      selfBinary(),
				LatestVersion:   fetchLatestReleaseVersion,
				Shell:           shell && !noShell,
				LockWorkspaces:  lockWorkspaces,
				BasePath:        basePath,
			})
			if err != nil {
				return err
			}

			var listener net.Listener
			if socket != "" {
				listener, err = webui.ListenUnix(socket)
				if err != nil {
					return err
				}
			} else {
				address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
				listener, err = net.Listen("tcp", address)
				if err != nil {
					return fmt.Errorf("listen on %s: %w", address, err)
				}
				if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
					port = tcp.Port
				}
			}

			base, cancelBase := context.WithCancel(context.Background())
			defer cancelBase()
			httpServer := &http.Server{
				Handler:           server.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				IdleTimeout:       120 * time.Second,
				BaseContext:       func(net.Listener) context.Context { return base },
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				<-ctx.Done()
				stop()
				fmt.Fprint(os.Stderr, locale.T("\n正在停止 ccl serve…（再按一次 Ctrl+C 立即退出）\n", "\nStopping ccl serve… (Ctrl+C again to quit now)\n"))
				cancelBase()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				_ = server.Close(shutdownCtx)
				_ = httpServer.Shutdown(shutdownCtx)
			}()

			if socket != "" {
				ready, _ := json.Marshal(map[string]any{"socket": listener.Addr().String(), "pid": os.Getpid(), "version": Version})
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ready)
				fmt.Fprintf(os.Stderr, locale.T(
					"ccl serve 已启动 · unix:%s · %d 个项目\n停止：Ctrl+C\n",
					"ccl serve listening · unix:%s · projects: %d\nStop with Ctrl+C\n",
				), listener.Addr().String(), server.WorkspaceCount())
			} else {
				baseURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + basePath + "/"
				launchURL := baseURL
				if generatedToken {
					launchURL = baseURL + "#" + token
				}
				fmt.Fprintf(os.Stderr, locale.T(
					"ccl serve 已启动 · %s · %d 个项目\n停止：Ctrl+C\n",
					"ccl serve listening · %s · projects: %d\nStop with Ctrl+C\n",
				), launchURL, server.WorkspaceCount())
				if openBrowser && !noOpen {
					if err := browser.Open(launchURL); err != nil {
						fmt.Fprintf(os.Stderr, locale.T(
							"ccl: 无法打开浏览器（%v）；请手动打开 %s\n",
							"ccl: could not open a browser (%v); open %s yourself\n",
						), err, launchURL)
					}
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
	flags.IntVar(&port, "port", 0, locale.T("本地回环监听端口（0 表示自动分配）", "Loopback TCP port to listen on (0 selects a free port)"))
	flags.BoolVar(&openBrowser, "open", true, locale.T("服务就绪后打开系统浏览器（--open=false 关闭）", "Open the system browser once ready (--open=false to disable)"))
	flags.BoolVar(&noOpen, "no-open", false, locale.T("不自动打开浏览器（覆盖 --open）", "Do not open a browser; overrides --open"))
	flags.StringArrayVar(&workspaces, "workspace", nil, locale.T("允许运行会话的项目目录（可重复；默认当前目录）", "Project folder conversations may run in (repeatable; default: current directory)"))
	flags.StringArrayVar(&workspaceRoots, "workspace-root", nil, locale.T("其直接子目录都作为项目的根目录（可重复）", "Directory whose subdirectories are all projects (repeatable)"))
	flags.StringVar(&token, "token", "", locale.T("/api 鉴权 token（默认读取 CCL_SERVE_TOKEN，未设置时自动生成）", "Token required by /api (defaults to CCL_SERVE_TOKEN, else generated per run)"))
	flags.IntVar(&maxConcurrency, "max-concurrency", 3, locale.T("最大并发 Grok Build 进程数", "Maximum simultaneous Grok Build processes"))
	flags.StringVarP(&providerName, "provider", "p", "", locale.T("新会话默认使用的 Provider（默认当前激活 Provider）", "Provider new conversations start with (default: active provider)"))
	flags.StringVar(&model, "model", "", locale.T("新会话默认模型 ID", "Model ID new conversations start with"))
	flags.StringVar(&effort, "effort", "", locale.T("新会话默认推理强度（low, medium, high, xhigh, max）", "Effort new conversations start with (low, medium, high, xhigh, max)"))
	flags.BoolVar(&shell, "shell", true, locale.T("在页面提供项目目录终端", "Offer a terminal (a shell in the project folder) in the page"))
	flags.BoolVar(&noShell, "no-shell", false, locale.T("禁用页面终端（覆盖 --shell）", "Do not offer a terminal in the page; overrides --shell"))
	flags.BoolVar(&lockWorkspaces, "lock-workspaces", false, locale.T("锁定项目列表，禁止从页面添加目录", "Serve only --workspace / --workspace-root folders; disallow adding others from the page"))
	flags.StringVar(&socket, "socket", "", locale.T("改为监听本机 Unix socket（不带路径时使用 ~/.ccl/serve/ccl.sock）", "Listen on a local Unix socket instead of TCP (no value: ~/.ccl/serve/ccl.sock)"))
	flags.StringVar(&basePath, "base-path", "", locale.T("挂载页面与 /api 的 URL 前缀，如 /chat", "Mount the page and /api under a URL prefix such as /chat"))
	flags.Lookup("socket").NoOptDefVal = defaultSocket
	return cmd
}

const defaultSocket = "default"

func socketPath(cmd *cobra.Command, socket string, args []string) (string, error) {
	if socket == defaultSocket && len(args) == 1 {
		socket, args = args[0], nil
	}
	if len(args) > 0 {
		return "", fmt.Errorf("unexpected argument %q", args[0])
	}
	if socket == "" {
		return "", nil
	}
	if cmd.Flags().Changed("port") {
		return "", errors.New("--socket cannot be combined with --port")
	}
	if socket == defaultSocket {
		return webui.DefaultSocketPath()
	}
	return socket, nil
}

func init() {
	rootCmd.AddCommand(newServeCommand())
}

func selfBinary() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
