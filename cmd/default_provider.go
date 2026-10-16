package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/locale"
)

// runClaudeWithoutProvider starts Claude Code exactly as its own
// configuration stands (the "default" selector), apart from ccl's launcher
// flags (bypass). A running ccl proxy's overlay is part of that configuration.
func runClaudeWithoutProvider(args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH: %w", err)
	}
	fmt.Fprintln(os.Stderr, locale.T(
		"ccl 使用 default：以 Claude Code 自身配置启动（ccl use <name> 可切换 provider）",
		"ccl is on default: starting Claude Code with its own configuration (ccl use <name> switches provider)",
	))
	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
