package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// newRetiredProviderToggleCommand keeps `ccl provider on|off` from older docs
// working: off is `ccl use default`; on cannot know which provider to restore.
func newRetiredProviderToggleCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:           use,
		Short:         "Retired: use ccl use default / ccl use <name>",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "off" {
				return runProviderUse(provider.DefaultProvider, false)
			}
			return errors.New(locale.T(
				"ccl provider on 已移除：用 ccl use <name> 选择 provider（ccl use default 使用 Claude Code 自身配置）",
				"ccl provider on was removed: choose a provider with ccl use <name> (ccl use default runs Claude Code's own configuration)",
			))
		},
	}
}

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
