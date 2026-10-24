package cmd

import (
	"context"
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
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/webui"
	"github.com/spf13/cobra"
)

func newServeCommand() *cobra.Command {
	var (
		port           int
		host           string
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
		Use:   "serve [--port 8099 | --socket [path]] [--workspace dir]... [--workspace-root dir]... [--host 127.0.0.1] [--token <token>]",
		Short: locale.T("在浏览器中使用 ccl 会话（HTTP + SSE，或本机 Unix socket）", "Use ccl conversations from a browser (HTTP + SSE, or a local Unix socket)"),
		Long: locale.T(`在浏览器中运行 ccl 会话：一个对话就是一个 ccl 终端实例（读写项目文件、运行工具、切换 provider）。

每一个回合起一个新的 Claude Code 进程，用 --resume 续接同一会话，所以 provider、模型、effort 和权限模式在回合边界生效，
停止就是把进程杀掉。会话历史存放在 ~/.ccl/serve/sessions/<id>/，重启后仍在。

页面参照 claude.ai/code：会话列表、消息流（diff、待办、计划、权限确认卡片）、文件与 Git 面板、设置弹窗
（provider、订阅登录、MCP 连接器）。设置通过 /api/config 读写 ~/.ccl/config.yaml；正在运行的回合不受影响。

项目：每个项目是一个文件夹。--workspace 可写多次；--workspace-root 下的每个子目录也都是项目（新 clone 的仓库
无需重启即出现）；都不写时用当前目录。页面的"项目"页列出所有项目，可以编辑各项目的 CLAUDE.md / CLAUDE.local.md，
也可以"打开其他文件夹"或"新建项目"，添加的文件夹保存在 ~/.ccl/serve/workspaces.json；
--lock-workspaces 禁止从页面添加，只用启动参数里的目录。

鉴权：绑定回环地址（默认 127.0.0.1）时不需要 token；--host 指定其它地址（如容器里的 0.0.0.0）时必须提供
--token 或 CCL_SERVE_TOKEN，否则拒绝启动。页面带网页终端（项目目录里的 shell），--no-shell 关闭。
默认在就绪后打开系统浏览器；容器内用 --no-open。

--socket：改为在 Unix socket 上提供同一套 HTTP 接口（给原生 App、脚本用），不占端口；不写路径时用
~/.ccl/serve/ccl.sock。socket 文件权限为 0600，只有当前用户能连，所以不需要 token；不会打开浏览器。
就绪后在 stdout 输出一行 JSON：{"socket":"<路径>","pid":<pid>,"version":"<版本>"}，其余日志都在 stderr。
同一路径上已有服务在运行时拒绝启动；上次异常退出留下的 socket 文件会被替换。

示例：
  ccl serve                                      # 本机，当前目录
  ccl serve --workspace-root ~/code              # ~/code 下的每个项目
  ccl serve --workspace ~/a --workspace ~/b --no-shell
  ccl serve --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"
  ccl serve --socket                             # ~/.ccl/serve/ccl.sock
  curl --unix-socket ~/.ccl/serve/ccl.sock http://localhost/api/sessions`, `Run ccl conversations in a browser: one conversation is one ccl terminal session (it reads and writes its
project, runs tools, and switches provider).

Every turn starts a new Claude Code process and resumes the same session with --resume, so provider, model,
effort and permission mode take effect at turn boundaries, and stopping a turn kills the process. History
lives in ~/.ccl/serve/sessions/<id>/ and survives restarts.

The page follows claude.ai/code: a session list, the thread (diffs, todo lists, plans, permission cards), file
and git panels, and a settings dialog (providers, subscription login, MCP connectors). Settings read and write
~/.ccl/config.yaml through /api/config; a running turn keeps the configuration it was started with.

Projects: each project is a folder. --workspace may be repeated, and every directory inside a --workspace-root
is a project too (a repository cloned there appears without a restart); with neither flag, the current
directory is the project. The page's Projects view lists them and edits each project's CLAUDE.md /
CLAUDE.local.md; it can also open another folder or create a new project. Added folders are kept in
~/.ccl/serve/workspaces.json. --lock-workspaces stops the page from adding folders.

Authentication: no token is needed while listening on a loopback address (the default, 127.0.0.1). Any other
--host, such as 0.0.0.0 in a container, requires --token or CCL_SERVE_TOKEN and refuses to start without one.
The page has a terminal (a shell in the project folder); --no-shell turns it off. The browser opens once the server is ready; use
--no-open in a container.

--socket serves the same HTTP API on a Unix socket instead (for native apps and scripts), so no port is
taken; without a path it is ~/.ccl/serve/ccl.sock. The socket is mode 0600, so only you can connect and no
token is needed; no browser opens. Once ready, one JSON line goes to stdout:
{"socket":"<path>","pid":<pid>,"version":"<version>"}; every other message goes to stderr. A path another
server still listens on is refused; a socket left behind by a crash is replaced.

Examples:
  ccl serve                                      # local, the current directory
  ccl serve --workspace-root ~/code              # every project in ~/code
  ccl serve --workspace ~/a --workspace ~/b --no-shell
  ccl serve --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"
  ccl serve --socket                             # ~/.ccl/serve/ccl.sock
  curl --unix-socket ~/.ccl/serve/ccl.sock http://localhost/api/sessions`),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := socketPath(cmd, socket, args)
			if err != nil {
				return err
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CCL_SERVE_TOKEN"))
			}
			// A loopback listener is reachable only from this machine, so it needs
			// no credential; anything else is exposed and must have one.
			// So is a Unix socket only this user may connect to.
			requiresToken := socket == "" && !isLoopbackHost(host)
			if requiresToken && strings.TrimSpace(token) == "" {
				return fmt.Errorf("--host %s is not a loopback address, so a token is required: pass --token or set CCL_SERVE_TOKEN", host)
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("--port must be between 1 and 65535, got %d", port)
			}
			if len(workspaces) == 0 && len(workspaceRoots) == 0 {
				workspaces = []string{"."}
			}
			if basePath, err = webui.NormalizeBasePath(strings.TrimSpace(basePath)); err != nil {
				return fmt.Errorf("--base-path: %w", err)
			}
			server, err := webui.New(webui.Config{
				Token:           token,
				RequireToken:    requiresToken,
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
				address := net.JoinHostPort(host, strconv.Itoa(port))
				listener, err = net.Listen("tcp", address)
				if err != nil {
					return fmt.Errorf("listen on %s: %w", address, err)
				}
			}
			// Every request context derives from base, so cancelling it ends
			// the event and terminal streams an open page holds forever.
			base, cancelBase := context.WithCancel(context.Background())
			defer cancelBase()
			httpServer := &http.Server{
				Handler:           server.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				// A turn streams for as long as Claude Code runs; there is no
				// write deadline that would not cut it off.
				IdleTimeout: 120 * time.Second,
				BaseContext: func(net.Listener) context.Context { return base },
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				<-ctx.Done()
				// A second Ctrl+C exits at once.
				stop()
				fmt.Fprint(os.Stderr, locale.T("\n正在停止 ccl serve…（再按一次 Ctrl+C 立即退出）\n", "\nStopping ccl serve… (Ctrl+C again to quit now)\n"))
				cancelBase()
				// Turns in flight are cancelled and stored; Claude Code gets
				// SIGINT and, after 2s, SIGKILL.
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = server.Close(shutdownCtx)
				_ = httpServer.Shutdown(shutdownCtx)
			}()

			if socket != "" {
				// The one line a native client waits for; everything else is stderr.
				ready, _ := json.Marshal(map[string]any{"socket": listener.Addr().String(), "pid": os.Getpid(), "version": Version})
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ready)
				fmt.Fprintf(os.Stderr, locale.T(
					"ccl serve 已启动 · unix:%s · %d 个项目\n停止：Ctrl+C\n",
					"ccl serve listening · unix:%s · projects: %d\nStop with Ctrl+C\n",
				), listener.Addr().String(), server.WorkspaceCount())
			} else {
				displayHost := host
				if displayHost == "0.0.0.0" || displayHost == "::" || displayHost == "" {
					displayHost = "127.0.0.1"
				}
				url := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port)) + basePath + "/"
				fmt.Fprintf(os.Stderr, locale.T(
					"ccl serve 已启动 · %s · %d 个项目\n停止：Ctrl+C\n",
					"ccl serve listening · %s · projects: %d\nStop with Ctrl+C\n",
				), url, server.WorkspaceCount())
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
	flags.StringArrayVar(&workspaces, "workspace", nil, "A project folder conversations may run in (repeatable; default: the current directory)")
	flags.StringArrayVar(&workspaceRoots, "workspace-root", nil, "A directory whose subdirectories are all projects (repeatable)")
	flags.StringVar(&token, "token", "", "Token required by /api, sent as Authorization: Bearer or X-CCL-Token (default: CCL_SERVE_TOKEN)")
	flags.IntVar(&maxConcurrency, "max-concurrency", 3, "Maximum simultaneous Claude Code processes")
	flags.StringVar(&providerName, "provider", "", "Provider new conversations start with (default: active provider)")
	flags.StringVar(&model, "model", "", "Model new conversations start with (slot or full id)")
	flags.StringVar(&effort, "effort", "", "Effort new conversations start with (low, medium, high, xhigh, max)")
	flags.BoolVar(&shell, "shell", true, "Offer a terminal (a shell in the project folder) in the page")
	flags.BoolVar(&noShell, "no-shell", false, "Do not offer a terminal in the page; overrides --shell")
	flags.BoolVar(&lockWorkspaces, "lock-workspaces", false, "Serve only the --workspace / --workspace-root folders; the page cannot add others")
	flags.StringVar(&socket, "socket", "", "Listen on this Unix socket instead of a TCP port (no value: ~/.ccl/serve/ccl.sock)")
	flags.StringVar(&basePath, "base-path", "", "Serve the page and /api under this URL prefix, such as /chat, behind a proxy that keeps the prefix (default: the root)")
	flags.Lookup("socket").NoOptDefVal = defaultSocket
	return cmd
}

// defaultSocket is what a bare --socket reads; it stands for
// webui.DefaultSocketPath.
const defaultSocket = "default"

// socketPath resolves --socket. A bare --socket takes no value of its own, so
// `--socket /path` leaves the path as the one argument; anything else is an
// unexpected argument. --host and --port do not apply to a socket.
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
	if cmd.Flags().Changed("host") || cmd.Flags().Changed("port") {
		return "", errors.New("--socket cannot be combined with --host or --port")
	}
	if socket == defaultSocket {
		return webui.DefaultSocketPath()
	}
	return socket, nil
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
	rootCmd.AddCommand(newServeCommand())
}

// selfBinary is this ccl executable, which the page runs for `ccl oauth`.
func selfBinary() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
