package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/claude-code-launch/ccl/internal/acp"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/spf13/cobra"
)

// acpQuietErrors tells Execute to print failures to stderr. ACP stdout is
// reserved for JSON-RPC frames.
var acpQuietErrors bool

var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: locale.T("在 stdio 上提供 Agent Client Protocol，供 Xcode 等客户端使用", "Speak Agent Client Protocol on stdio for Xcode and other ACP clients"),
	Long: locale.T(`在 stdio 上提供 Agent Client Protocol（ACP v1），把 CCL 暴露成一个 Claude 风格 Agent。

Xcode 27 配置（Settings → Intelligence → Agents → Add an Agent）：
  Name:        CCL
  Executable:  ccl 的绝对路径（which ccl；Xcode 不展开 ~，也不搜 PATH）
  Arguments:   acp
  Interpreter: 留空

先运行 ccl use --acp <name>，让 ACP 直接使用同一份 Provider 配置；无需维护 ACP 专用副本。
运行中切换 ACP 使用的 Provider 或修改该配置，会在下一条 prompt 前恢复会话并生效，无需重启 Xcode Agent。
工具调用、diff、图片和 Xcode MCP 会转给 Claude Code；权限走 session/request_permission，不使用 ccl bypass。
没有直播终端（Bash 只作为 tool_call execute）。
stdout 只输出 JSON-RPC；日志走 stderr。`, `Speak Agent Client Protocol (ACP v1) on stdio so Xcode can drive CCL as a Claude-style agent.

Xcode 27 setup (Settings → Intelligence → Agents → Add an Agent):
  Name:        CCL
  Executable:  absolute path to ccl (which ccl; Xcode does not expand ~ or search PATH)
  Arguments:   acp
  Interpreter: leave blank

Select a shared provider configuration with ccl use --acp <name>; ACP does not keep a separate provider copy.
ACP selection or provider configuration changes take effect by resuming the session before the next prompt; restarting the Xcode agent is not required.
Tool calls, diffs, images, and Xcode MCP servers are forwarded to Claude Code.
Permissions go through session/request_permission (ccl bypass does not apply).
There is no live terminal (Bash is a tool_call with kind=execute).
stdout is JSON-RPC only; logs go to stderr.`),
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	PreRun: func(cmd *cobra.Command, args []string) {
		acpQuietErrors = true
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		acpQuietErrors = true
		return runACP(cmd.Context(), os.Stdin, os.Stdout, os.Stderr)
	},
}

func runACP(ctx context.Context, in io.Reader, out, errw io.Writer) error {
	if !IsInstalled() {
		return fmt.Errorf("claude CLI not found in PATH; install Claude Code first")
	}
	manager := newACPLaunchManager()
	if err := manager.primeContext(ctx); err != nil {
		return err
	}
	defer manager.Close()
	storePath, err := acp.DefaultSessionStorePath()
	if err != nil {
		return err
	}

	return acp.Serve(ctx, in, out, acp.Config{
		AgentName:        "ccl",
		AgentTitle:       "CCL",
		AgentVersion:     Version,
		Stderr:           errw,
		SessionStorePath: storePath,
		AcquireLaunch: func(ctx context.Context) (acp.LaunchLease, error) {
			return manager.acquireContext(ctx)
		},
	})
}

func init() {
	rootCmd.AddCommand(acpCmd)
}
