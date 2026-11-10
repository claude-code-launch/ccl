package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// "default" defers to the client.
var effortLevels = []string{"low", "medium", "high", "xhigh", "default"}

func newProviderEffortCommand() *cobra.Command {
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:       "effort [low|medium|high|xhigh|default]",
		Short:     "Set the effort a provider's sessions start at",
		Long:      "Set the default reasoning effort for Grok Build. Use default to clear it.",
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
