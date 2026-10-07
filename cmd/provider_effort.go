package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// effortLevels are Claude Code's effortLevel values; "default" clears ccl's.
var effortLevels = []string{"low", "medium", "high", "xhigh", "default"}

func newProviderEffortCommand() *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   "effort [low|medium|high|xhigh|default]",
		Short: "Set the effort a provider's sessions start at",
		Long: `Set the reasoning effort a provider's Claude Code sessions start at.

ccl passes it as the effortLevel setting, so /effort still changes it during a
session. "default" leaves effort to Claude Code. Without an argument, prints
the current value.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: effortLevels,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderEffort(cmd.OutOrStdout(), args, *target)
		},
	}
	addProviderTargetFlags(cmd, target)
	return cmd
}

func runProviderEffort(out io.Writer, args []string, target providerTarget) error {
	name, p, err := target.load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintf(out, "%s: effort %s\n", name, providerEffortSummary(p))
		return nil
	}
	level := strings.ToLower(strings.TrimSpace(args[0]))
	valid := false
	for _, candidate := range effortLevels {
		valid = valid || candidate == level
	}
	if !valid {
		return fmt.Errorf("expected one of %s, got %q", strings.Join(effortLevels, ", "), args[0])
	}
	if level == "default" {
		level = ""
	}
	if err := updateProvider(name, func(p *provider.Provider) error {
		p.EffortLevel = level
		return nil
	}); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}
	p.EffortLevel = level
	fmt.Fprintf(out, "%s: effort %s\n", name, providerEffortSummary(p))
	return nil
}

func newProviderUltracodeCommand() *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   "ultracode on|off",
		Short: "Turn Claude Code's ultracode on or off for a provider",
		Long: `Turn Claude Code's ultracode setting on or off for a provider's sessions:
with it on, Claude plans a workflow for each substantive task.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderUltracode(cmd.OutOrStdout(), args[0], *target)
		},
	}
	addProviderTargetFlags(cmd, target)
	return cmd
}

func runProviderUltracode(out io.Writer, value string, target providerTarget) error {
	enabled, ok := parseBypassOnOff(value)
	if !ok {
		return fmt.Errorf("expected on or off, got %q", value)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	name, err := target.resolve(cfg)
	if err != nil {
		return err
	}
	if err := updateProvider(name, func(p *provider.Provider) error {
		p.Ultracode = enabled
		return nil
	}); err != nil {
		return fmt.Errorf("save ccl config: %w", err)
	}
	fmt.Fprintf(out, locale.T("%s: ultracode %s\n", "%s: ultracode %s\n"), name, onOff(enabled))
	return nil
}
