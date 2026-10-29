package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// runClaudeWithoutProvider starts Claude Code exactly as its own
// configuration stands (the "default" selector), apart from ccl's launcher
// flags (bypass). A running ccl proxy's overlay is part of that configuration.
func runClaudeWithoutProvider(args []string) error {
	return runClaudeWithProfile("", "", args)
}

func runClaudeAccount(p provider.Provider, args []string) error {
	if err := claudeaccount.ValidateProvider(p); err != nil {
		return err
	}
	if err := claudeaccount.CheckArgs(args); err != nil {
		return err
	}
	store, err := claudeaccount.Open()
	if err != nil {
		return err
	}
	profile, err := store.Profile(p.OAuthAccountCredential)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := claudeaccount.CheckSettings(profile, cwd); err != nil {
		return err
	}
	return runClaudeWithProfile(p.Name, profile, args)
}

func runClaudeWithProfile(name, profile string, args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH: %w", err)
	}
	if name != "" {
		fmt.Fprintf(os.Stderr, locale.T("ccl 使用 Claude 账号 %q\n", "ccl is using Claude account %q\n"), name)
	} else {
		fmt.Fprintln(os.Stderr, locale.T(
			"ccl 使用 default：以 Claude Code 自身配置启动（ccl use <name> 可切换 provider）",
			"ccl is on default: starting Claude Code with its own configuration (ccl use <name> switches provider)",
		))
	}
	cmd := process.Command(claudePath, args...)
	if name != "" {
		cmd.Env = claudeaccount.Environment(os.Environ(), profile)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
