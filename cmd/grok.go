package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

const grokAlwaysApproveFlag = "--always-approve"

func lookGrokBinary() (string, error) {
	return groklaunch.LookPath()
}

var grokSubcommands = []string{
	"agent", "clone", "completions", "dashboard", "disk-usage", "doctor", "du",
	"export", "help", "inspect", "leader", "login", "logout", "mcp", "memory",
	"models", "plugin", "sessions", "setup", "ssh", "trace", "update", "usage",
	"v", "version", "worker", "worktree", "wrap",
}

var grokLocalSubcommands = map[string]struct{}{
	"clone": {}, "completions": {}, "disk-usage": {}, "doctor": {}, "du": {},
	"export": {}, "help": {}, "login": {}, "logout": {}, "mcp": {}, "memory": {},
	"plugin": {}, "sessions": {}, "setup": {}, "trace": {}, "update": {}, "usage": {},
	"v": {}, "version": {}, "worktree": {},
}

var grokValueFlags = map[string]struct{}{
	"--add-dir": {}, "--agent": {}, "--agents": {}, "--allow": {}, "--allowed-tools": {}, "--allowedTools": {},
	"--append-system-prompt": {}, "--betas": {}, "--cwd": {}, "--debug-file": {},
	"--deny": {}, "--disallowed-tools": {}, "--disallowedTools": {},
	"--effort": {}, "--reasoning-effort": {}, "--fallback-model": {},
	"--file": {}, "--from-pr": {}, "--input-format": {}, "--json-schema": {},
	"--leader-socket": {}, "--log-level": {}, "--max-budget-usd": {}, "--max-turns": {},
	"--mcp-config": {}, "--model": {}, "--name": {}, "--output-format": {},
	"--permission-mode": {}, "--permission-prompt-tool": {}, "--plugin-dir": {},
	"--prompt": {}, "--prompt-file": {}, "--prompt-json": {}, "--single": {},
	"--ref": {}, "--worktree-ref": {}, "--rules": {},
	"--sandbox": {}, "--session-id": {}, "--setting-sources": {}, "--settings": {},
	"--system-prompt": {}, "--system-prompt-override": {}, "--teleport": {}, "--tools": {},
	"-C": {}, "-m": {}, "-n": {}, "-p": {}, "-s": {},
}

func isGrokSubcommand(arg string) bool {
	for _, name := range grokSubcommands {
		if arg == name {
			return true
		}
	}
	return false
}

func isGrokSessionFlag(flag string) bool {
	switch flag {
	case "-p", "--prompt", "--prompt-file", "--prompt-json", "--single",
		"-c", "--continue", "-r", "--resume", "-s", "--session-id",
		"-w", "--worktree", "--worktree-ref", "--ref",
		"--agent", "--agents", "--from-pr", "--teleport":
		return true
	default:
		return false
	}
}

func inspectGrokInvocation(args []string) (subcmd string, infoOnly bool) {
	sessionMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "-h" || arg == "--help" || arg == "-v" || arg == "--version" {
			infoOnly = true
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name, _, hasInlineValue := strings.Cut(arg, "=")
			if isGrokSessionFlag(name) {
				sessionMode = true
			}
			if _, needsValue := grokValueFlags[name]; needsValue && !hasInlineValue {
				i++
				continue
			}
			// Handle attached short value flags such as -p"hi" or -mgrok-4.
			if len(arg) > 2 && !strings.HasPrefix(arg, "--") {
				short := arg[:2]
				if isGrokSessionFlag(short) {
					sessionMode = true
				}
			}
			continue
		}
		if !sessionMode && subcmd == "" {
			if isGrokSubcommand(arg) {
				subcmd = arg
			} else {
				sessionMode = true
			}
		}
	}
	return subcmd, infoOnly
}

func isLocalGrokInvocation(args []string) bool {
	subcmd, infoOnly := inspectGrokInvocation(args)
	if infoOnly {
		return true
	}
	if subcmd == "" {
		return false
	}
	_, ok := grokLocalSubcommands[subcmd]
	return ok
}

func applyGrokBypassMode(args []string, enabled bool) []string {
	out := append([]string(nil), args...)
	if !enabled {
		return out
	}
	for _, arg := range out {
		if arg == "--" {
			break
		}
		if arg == grokAlwaysApproveFlag || arg == "--yolo" ||
			arg == "--permission-mode" || strings.HasPrefix(arg, "--permission-mode=") {
			return out
		}
	}
	subcmd, infoOnly := inspectGrokInvocation(out)
	if infoOnly || subcmd != "" {
		return out
	}
	return append([]string{grokAlwaysApproveFlag}, out...)
}

func runGrokAgent(cfg *provider.Config, name string, args []string) error {
	return runGrokAgentWithOptions(cfg, name, false, args)
}

func runGrokAgentWithOptions(cfg *provider.Config, name string, explicitProvider bool, args []string) error {
	var p provider.Provider
	native := provider.IsDefaultProvider(name)
	if native && strings.TrimSpace(cfg.LegacyClaudeDefault) != "" {
		return fmt.Errorf("%s", locale.T(
			"旧的默认 Claude 账号已退役。请运行 ccl use default 切换到 Grok Build 自身登录，或选择其他 Provider",
			"the default Claude account has been retired. Run ccl use default to select Grok Build's own login, or choose another provider",
		))
	}
	localOnly := isLocalGrokInvocation(args)
	if !native && (!localOnly || explicitProvider) {
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
	if native || localOnly {
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
