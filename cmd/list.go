package cmd

import (
	"fmt"
	"io"
	"sort"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var listShowAll bool

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		return printProviders(cmd.OutOrStdout(), cfg, listShowAll, "No providers added yet. Use 'ccl set' to add one.", "Registered providers:")
	},
}

func printProviders(out io.Writer, cfg *provider.Config, showAll bool, emptyMessage, heading string) error {
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(out, emptyMessage)
		return nil
	}

	var names []string
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(out, heading)
	for _, name := range names {
		mark := " "
		if name == cfg.ActiveProvider {
			mark = "*"
		}
		p := cfg.Providers[name]
		fmt.Fprintf(
			out,
			"%s %s (%s, auth: %s, effort: %s, 1M: %s, model: %s)\n",
			mark,
			name,
			provider.ProtocolLabel(p.Type),
			providerAuthLabel(p),
			providerEffortSummary(p),
			providerOneMSummary(p),
			formatModelList(p.Model, showAll),
		)
	}

	return nil
}

func init() {
	listCmd.Flags().BoolVarP(&listShowAll, "all", "a", false, "Show all models for each provider")
	rootCmd.AddCommand(listCmd)
}
