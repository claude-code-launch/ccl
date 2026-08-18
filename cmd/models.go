package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/protocol"
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
	p, runtime, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		return err
	}
	defer cleanup()

	catalog := fetchModelInfosForProvider(p)
	modelsStr := p.Model
	source := "configured model pool"
	if showAll || modelsStr == "" {
		fetched := modelIDs(catalog)
		if len(fetched) == 0 {
			fetched = runtime.Models()
		}
		if len(fetched) == 0 {
			fetched = fetchModelsForProvider(p)
		}
		if len(fetched) == 0 {
			if modelsStr == "" || showAll {
				return fmt.Errorf("no models found from provider")
			}
		} else {
			modelsStr = strings.Join(fetched, ",")
			source = "provider catalog"
		}
	}

	modelList := parseModelList(modelsStr)
	if len(modelList) == 0 {
		fmt.Println("No models found.")
		return nil
	}

	fmt.Printf("Models · %s\n", p.Name)
	fmt.Printf("Source: %s · %d model(s)\n\n", source, len(modelList))

	availableSet := testModelsConcurrently(ctx, modelList, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth, p.ModelProtocols)
	available, unavailable := classifyModels(modelList, availableSet)
	fmt.Println()
	printModelReportWithMetadata(available, unavailable, indexModelInfos(catalog))

	return nil
}

func modelIDs(infos []protocol.ModelInfo) []string {
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		if id := strings.TrimSpace(info.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func indexModelInfos(infos []protocol.ModelInfo) map[string]protocol.ModelInfo {
	indexed := make(map[string]protocol.ModelInfo, len(infos))
	for _, info := range infos {
		if id := strings.TrimSpace(info.ID); id != "" {
			indexed[strings.ToLower(id)] = info
		}
	}
	return indexed
}

func init() {
	rootCmd.AddCommand(modelsCmd)
}
