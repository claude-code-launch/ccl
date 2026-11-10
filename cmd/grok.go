package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

const grokAlwaysApproveFlag = "--always-approve"

func lookGrokBinary() (string, error) {
	path, err := exec.LookPath("grok")
	if err == nil {
		return path, nil
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if local, localErr := exec.LookPath(filepath.Join(home, ".grok", "bin", "grok")); localErr == nil {
			return local, nil
		}
	}
	return "", err
}

var grokSubcommands = []string{
	"agent", "clone", "completions", "dashboard", "disk-usage", "doctor", "du",
	"export", "help", "inspect", "leader", "login", "logout", "mcp", "memory",
	"models", "plugin", "sessions", "setup", "ssh", "trace", "update", "usage",
	"v", "version", "worker", "worktree", "wrap",
}

func isGrokSubcommand(arg string) bool {
	for _, name := range grokSubcommands {
		if arg == name {
			return true
		}
	}
	return false
}

func applyGrokBypassMode(args []string, enabled bool) []string {
	out := append([]string(nil), args...)
	if !enabled {
		return out
	}
	for _, arg := range out {
		if arg == grokAlwaysApproveFlag || arg == "--yolo" ||
			arg == "--permission-mode" || strings.HasPrefix(arg, "--permission-mode=") {
			return out
		}
	}
	if len(out) > 0 && isGrokSubcommand(out[0]) {
		return out
	}
	return append([]string{grokAlwaysApproveFlag}, out...)
}

func runGrokAgent(cfg *provider.Config, name string, args []string) error {
	var p provider.Provider
	native := provider.IsDefaultProvider(name)
	if native && strings.TrimSpace(cfg.LegacyClaudeDefault) != "" {
		return fmt.Errorf("%s", locale.T(
			"旧的默认 Claude 账号已退役。请运行 ccl use default 切换到 Grok Build 自身登录，或选择其他 Provider",
			"the default Claude account has been retired. Run ccl use default to select Grok Build's own login, or choose another provider",
		))
	}
	if !native {
		var err error
		p, err = resolveProviderFromConfig(cfg, name)
		if err != nil {
			return err
		}
		if err := provider.ValidateRuntime(p); err != nil {
			return err
		}
	}
	grokPath, err := lookGrokBinary()
	if err != nil {
		return fmt.Errorf("%s", locale.T(
			"未找到 Grok Build CLI。请先安装 Grok Build（https://x.ai/cli）",
			"Grok Build CLI not found. Install Grok Build first (https://x.ai/cli)",
		))
	}
	args = applyGrokBypassMode(args, cfg.BypassMode)
	if native {
		cmd := process.Command(grokPath, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return groklaunch.RunCommand(cmd)
	}
	oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
	defer func() {
		if logPath := oauthproxy.LogFilePath(); logPath != "" {
			fmt.Fprintf(os.Stderr, "\n[ccl log] run finished · file: %s\n", logPath)
			oauthproxy.CloseLog()
		}
	}()
	return groklaunch.Run(grokPath, p, args)
}
