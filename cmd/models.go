package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var modelsCmd = newModelsCommand("models")

func newModelsCommand(use string) *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "List available models with availability status",
		Long: `List and probe models for the active provider.

Without --all, tests the configured model pool (provider.Model). With --all,
fetches the upstream catalog (or OAuth runtime models) and tests those instead.

Examples:
  ccl models
  ccl models --all
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModels(cmd.Context(), showAll)
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Show all provider models (not just configured ones)")
	return cmd
}

func runModels(ctx context.Context, showAll bool) error {
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	p, _, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		return err
	}
	defer cleanup()

	modelsStr := p.Model
	source := "configured model pool"
	if showAll || modelsStr == "" {
		fetched := fetchModelsForProvider(p)
		if len(fetched) == 0 {
			if modelsStr == "" || showAll {
				return fmt.Errorf("no models found from provider")
			}
		} else {
			modelsStr = strings.Join(fetched, ",")
			source = "provider API"
		}
	}

	modelList := parseModelList(modelsStr)
	if len(modelList) == 0 {
		fmt.Println("No models found.")
		return nil
	}

	fmt.Printf("Models · %s\n", p.Name)
	fmt.Printf("Source: %s · %d model(s)\n\n", source, len(modelList))

	availableSet := testModelsConcurrently(ctx, modelList, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
	available, unavailable := classifyModels(modelList, availableSet)
	fmt.Println()
	printModelReport(available, unavailable)

	return nil
}

func init() {
	rootCmd.AddCommand(modelsCmd)
}
