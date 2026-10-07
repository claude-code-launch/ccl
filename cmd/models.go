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
	var showAll, probe bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "List the active provider's models, optionally probing each",
		Long: `List models for the active provider.

Without --all, lists the configured model pool (provider.Model). With --all,
lists the upstream catalog (or OAuth runtime models) instead.

--probe sends a 1-token request to every listed model and reports which
answer. That is a real request per model and counts against your plan
(Copilot premium requests, Qoder credits, API usage).

Examples:
  ccl provider models
  ccl provider models --all
  ccl provider models --probe
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModels(cmd.Context(), showAll, probe)
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Show all provider models (not just configured ones)")
	cmd.Flags().BoolVar(&probe, "probe", false, "Send a 1-token request to each model to test availability (billed)")
	return cmd
}

func runModels(ctx context.Context, showAll, probe bool) error {
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	configured := p
	p, runtime, cleanup, err := prepareProviderRuntime(context.Background(), p)
	if err != nil {
		return err
	}
	defer cleanup()

	catalog := fetchModelInfosForProvider(p)
	modelsStr := p.Model
	source := "configured model pool"
	if showAll || modelsStr == "" {
		fetched := modelIDs(catalog)
		if len(fetched) == 0 && runtime != nil {
			// Only proxy-backed providers (OpenAI-compatible, models.dev, or
			// OAuth) start a runtime; a direct Anthropic API-key provider
			// runs without one, so runtime is nil and must not be dereferenced.
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
			if runtime != nil && runtime.ModelCatalogIsFallback() {
				// The runtime could not reach the account's catalog and served a
				// built-in compatibility list; saying "provider catalog" would
				// present that guess as fact.
				source = "built-in fallback (provider catalog unavailable)"
			}
		}
	}

	modelList := parseModelList(modelsStr)
	if len(modelList) == 0 {
		fmt.Println("No models found.")
		return nil
	}

	// Probe the same target ccl set uses: API-key providers are verified on
	// their configured upstream (the loopback runtime has no Chat/Responses
	// route), OAuth subscriptions on their loopback runtime.
	probeTarget := modelProbeTarget(configured, p)

	fmt.Printf("Models · %s\n", p.Name)
	fmt.Printf("Source: %s · %d model(s)\n\n", source, len(modelList))

	metadata := indexModelInfos(catalog)
	if !probe {
		for _, model := range modelList {
			fmt.Printf("  · %s\n", modelReportLabel(model, metadata))
		}
		fmt.Printf("\nNot probed. `ccl provider models --probe` sends a 1-token request to each of the %d model(s) (billed).\n", len(modelList))
		return nil
	}
	fmt.Printf("Probing %d model(s) with a 1-token request each...\n\n", len(modelList))

	availableSet := testModelsConcurrently(ctx, modelList, probeTarget.Endpoint, probeTarget.APIKey, probeWireType(probeTarget), probeTarget.AnthropicAuth, probeTarget.ModelProtocols)
	available, unavailable := classifyModels(modelList, availableSet)
	fmt.Println()
	printModelReportWithMetadata(available, unavailable, metadata)

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
	rootCmd.AddCommand(deprecatedRootAlias(modelsCmd, "ccl provider models"))
}
