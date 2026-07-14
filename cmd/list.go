package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var lsCmd = newProviderListCommand("ls")

func newProviderListCommand(use string) *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "List registered providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			return printProviders(cmd.OutOrStdout(), cfg, showAll, "No providers added yet. Use 'ccl set' to add one.", "Registered providers:")
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Show detailed providers with full model pools")
	return cmd
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
	if showAll {
		return printProviderDetails(out, cfg, names)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, " \tNAME\tTYPE\tAUTH\tEFFORT\t1M\tMODELS\tSLOTS")
	for _, name := range names {
		mark := " "
		if name == cfg.ActiveProvider {
			mark = "*"
		}
		p := cfg.Providers[name]
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			mark,
			name,
			provider.ProtocolLabel(p.Type),
			providerAuthLabel(p),
			providerEffortSummary(p),
			providerOneMShortSummary(p),
			formatModelCount(p.Model),
			formatSlotCount(p),
		)
	}

	return tw.Flush()
}

func printProviderDetails(out io.Writer, cfg *provider.Config, names []string) error {
	for i, name := range names {
		mark := " "
		if name == cfg.ActiveProvider {
			mark = "*"
		}
		p := cfg.Providers[name]
		fmt.Fprintf(out, "%s %s\n", mark, name)
		fmt.Fprintf(out, "    Type     : %s\n", provider.ProtocolLabel(p.Type))
		fmt.Fprintf(out, "    Auth     : %s\n", providerAuthLabel(p))
		if p.OAuthProvider != "" {
			fmt.Fprintf(out, "    OAuth    : %s\n", p.OAuthProvider)
		}
		fmt.Fprintf(out, "    Endpoint : %s\n", p.Endpoint)
		fmt.Fprintf(out, "    Effort   : %s\n", providerEffortSummary(p))
		fmt.Fprintf(out, "    1M       : %s\n", providerOneMSummary(p))
		fmt.Fprintf(out, "    Models   : %s\n", formatModelCount(p.Model))
		fmt.Fprintf(out, "    Slots    : %s\n", formatSlotSummaryLong(p))
		if p.Model != "" {
			fmt.Fprintf(out, "    Pool     : %s\n", p.Model)
		}
		if i < len(names)-1 {
			fmt.Fprintln(out)
		}
	}
	return nil
}

func formatModelCount(modelStr string) string {
	count := len(parseModelList(modelStr))
	if count == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", count)
}

func providerOneMShortSummary(p provider.Provider) string {
	replacer := strings.NewReplacer(
		"opus", "O",
		"sonnet", "S",
		"haiku", "H",
		"custom", "C",
		"enabled", "on",
	)
	return replacer.Replace(providerOneMSummary(p))
}

func formatSlotSummaryLong(p provider.Provider) string {
	parts := compactSlotParts(p)
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func formatSlotCount(p provider.Provider) string {
	configured := 0
	for _, model := range []string{p.OpusModel, p.SonnetModel, p.HaikuModel, p.CustomModelID, p.SubagentModel} {
		if stripOneMSuffix(model) != "" {
			configured++
		}
	}
	return fmt.Sprintf("%d/5", configured)
}

func compactSlotParts(p provider.Provider) []string {
	slots := []struct {
		label string
		model string
	}{
		{"O", p.OpusModel},
		{"S", p.SonnetModel},
		{"H", p.HaikuModel},
		{"C", p.CustomModelID},
		{"A", p.SubagentModel},
	}

	var parts []string
	for _, slot := range slots {
		model := stripOneMSuffix(slot.model)
		if model == "" {
			continue
		}
		parts = append(parts, slot.label+":"+model)
	}
	return parts
}

func init() {
	rootCmd.AddCommand(lsCmd)
}
