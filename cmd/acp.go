package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/claude-code-launch/ccl/internal/acp"
	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/spf13/cobra"
)

// acpQuietErrors tells Execute to print failures to stderr. ACP stdout is
// reserved for JSON-RPC frames.
var acpQuietErrors bool

var acpCmd = &cobra.Command{
	Use:   "acp [-- adapter-options]",
	Short: locale.T("为 claude-agent-acp 注入 CCL 配置，并透传 ACP", "Wrap claude-agent-acp with CCL configuration and pass through ACP"),
	Long: locale.T(`ACP 协议由 claude-agent-acp 完整实现；CCL 只处理 provider、模型、环境和代理配置。

先安装 Node.js >=22 和上游适配器：
  npm install -g @agentclientprotocol/claude-agent-acp
适配器通过 Claude Agent SDK 自带的 Claude 运行，无需单独安装 Claude CLI。

Xcode 27 配置（Settings → Intelligence → Agents → Add an Agent）：
  Name:        CCL
  Executable:  ccl 的绝对路径（which ccl；Xcode 不展开 ~，也不搜 PATH）
  Arguments:   acp
  Interpreter: 留空

先运行 ccl use --acp <name>，让 ACP 直接使用同一份 Provider 配置；无需维护 ACP 专用副本。
运行中修改该配置，会在下一条空闲 prompt 前通过上游 session/resume 更新，无需重启 Agent。
模型、模式、工具、权限、终端、MCP、会话恢复及扩展以适配器和客户端协商的能力为准。
用 --adapter <绝对路径> 或 CCL_ACP_ADAPTER 指定 GUI 可访问的适配器入口；-- 后的选项透传上游。
stdout 只输出上游 ACP；日志走 stderr。`, `The complete ACP implementation is claude-agent-acp. CCL only supplies provider, model, environment and proxy configuration.

Install Node.js >=22 and the upstream adapter first:
  npm install -g @agentclientprotocol/claude-agent-acp
The adapter uses the Claude executable shipped with the Agent SDK; a separate Claude CLI is not required.

Xcode 27 setup (Settings → Intelligence → Agents → Add an Agent):
  Name:        CCL
  Executable:  absolute path to ccl (which ccl; Xcode does not expand ~ or search PATH)
  Arguments:   acp
  Interpreter: leave blank

Select a shared provider configuration with ccl use --acp <name>; ACP does not keep a separate provider copy.
Configuration changes take effect through upstream session/resume before the next idle prompt; restarting the agent is not required.
Models, modes, tools, permissions, terminals, MCP, session history and extensions are negotiated by the adapter and client.
Use --adapter <absolute-path> or CCL_ACP_ADAPTER for a GUI-accessible adapter entry. Options after -- are passed to the adapter.
stdout is upstream ACP only; logs go to stderr.`),
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 && cmd.ArgsLenAtDash() < 0 {
			return fmt.Errorf("adapter options must follow --")
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	PreRun: func(cmd *cobra.Command, args []string) {
		acpQuietErrors = true
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		acpQuietErrors = true
		adapter, _ := cmd.Flags().GetString("adapter")
		return runACP(cmd.Context(), os.Stdin, os.Stdout, os.Stderr, adapter, args)
	},
}

func runACP(ctx context.Context, in io.Reader, out, errw io.Writer, adapter string, args []string) error {
	if adapter == "" {
		adapter = os.Getenv("CCL_ACP_ADAPTER")
	}
	if adapter == "" {
		adapter = "claude-agent-acp"
	}
	adapterPath, err := exec.LookPath(adapter)
	if err != nil {
		return fmt.Errorf("claude-agent-acp not found: install Node.js >=22 and run npm install -g @agentclientprotocol/claude-agent-acp, or use ccl acp --adapter <absolute-path>: %w", err)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("Node.js >=22 is required by claude-agent-acp: %w", err)
	}
	ctx, stop := process.SignalContext(ctx)
	defer stop()
	manager := newACPLaunchManager()
	defer manager.Close()
	initial, err := manager.acquireContext(ctx)
	if err != nil {
		return err
	}
	defer initial.Release()
	cwd, _ := os.Getwd()
	options, err := initial.Options(cwd)
	if err != nil {
		return err
	}
	env, err := claude.AgentProcessEnv(options)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return acp.Serve(ctx, in, out, acp.Config{
		NodePath: nodePath, AdapterPath: adapterPath, Args: args, Env: env, Stderr: errw,
		LegacyStore: filepath.Join(home, ".ccl", "acp", "sessions"),
		AcquireLaunch: func(ctx context.Context) (acp.LaunchLease, error) {
			return manager.acquireContext(ctx)
		},
	})
}

func init() {
	acpCmd.Flags().String("adapter", "", locale.T("claude-agent-acp 入口的路径", "Path to the claude-agent-acp entry"))
	rootCmd.AddCommand(acpCmd)
}
