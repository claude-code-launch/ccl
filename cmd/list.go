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
		Long: `List providers from ~/.ccl/config.yaml.

A single-account OAuth provider whose credential already belongs to an auth
group is never listed: the group's own provider represents it. -a/--all does not
change that, it switches the table for a detailed view with full model pools and
slot details. To see what a group is made of, use ccl oauth group ls, or read
~/.ccl/config.yaml.

Examples:
  ccl ls
  ccl ls -a
  ccl provider ls
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			return printProviders(cmd.OutOrStdout(), cfg, showAll, "No providers added yet. Use 'ccl set' to add one.", "Registered providers:")
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Detailed view with full model pools (does not reveal auth group members)")
	return cmd
}

func printProviders(out io.Writer, cfg *provider.Config, showAll bool, emptyMessage, heading string) error {
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(out, emptyMessage)
		return nil
	}

	groupCredentials := make(map[string]bool)
	for _, group := range cfg.AuthGroups {
		for _, credential := range group.Credentials {
			credential = strings.ToLower(strings.TrimSpace(credential))
			if credential != "" {
				groupCredentials[credential] = true
			}
		}
	}

	var names []string
	for name, p := range cfg.Providers {
		credential := strings.ToLower(strings.TrimSpace(p.OAuthAccountCredential))
		if credential != "" && groupCredentials[credential] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(out, emptyMessage)
		return nil
	}

	fmt.Fprintln(out, heading)
	if showAll {
		return printProviderDetails(out, cfg, names)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, " \tNAME\tKIND\tTYPE\tAUTH\tEFFORT\tCONTEXT\tMODELS\tSLOTS")
	for _, name := range names {
		mark := " "
		if name == cfg.ActiveProvider {
			mark = "*"
		}
		p := cfg.Providers[name]
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			mark,
			name,
			providerKindLabel(p),
			provider.ProtocolLabelForProvider(p),
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
		fmt.Fprintf(out, "    Kind     : %s\n", providerKindLabel(p))
		fmt.Fprintf(out, "    Type     : %s\n", provider.ProtocolLabelForProvider(p))
		fmt.Fprintf(out, "    Auth     : %s\n", providerAuthLabel(p))
		if p.AuthGroup != "" {
			fmt.Fprintf(out, "    Group    : %s (%d account(s))\n", p.AuthGroup, len(p.OAuthAccountCredentials))
		}
		if p.OAuthProvider != "" {
			fmt.Fprintf(out, "    OAuth    : %s\n", p.OAuthProvider)
		}
		fmt.Fprintf(out, "    Endpoint : %s\n", p.Endpoint)
		fmt.Fprintf(out, "    Effort   : %s\n", providerEffortSummary(p))
		fmt.Fprintf(out, "    Fast     : %s\n", providerFastSummary(p))
		fmt.Fprintf(out, "    Context  : %s\n", providerOneMSummary(p))
		fmt.Fprintf(out, "    Models   : %s\n", formatModelCount(p.Model))
		fmt.Fprintf(out, "    Slot IDs : %s\n", formatSlotSummaryLong(p))
		if p.Model != "" {
			fmt.Fprintf(out, "    Pool IDs : %s\n", p.Model)
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
