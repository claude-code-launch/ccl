package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var modelsShowAll bool

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List available models with availability status",
	Long:  `List all available models for the active provider and test each for availability.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := resolveProvider()
		if err != nil {
			return err
		}

		modelsStr := p.Model
		if modelsShowAll || modelsStr == "" {
			fetched := fetchModelsForProvider(p)
			if len(fetched) == 0 {
				if modelsStr == "" || modelsShowAll {
					return fmt.Errorf("no models found from provider")
				}
			} else {
				modelsStr = strings.Join(fetched, ",")
			}
		}

		modelList := parseModelList(modelsStr)
		if len(modelList) == 0 {
			fmt.Println("No models found.")
			return nil
		}

		displayModels := modelList

		// Test each model concurrently
		availableSet := testModelsConcurrently(displayModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)

		// Display results
		available, unavailable := classifyModels(displayModels, availableSet)
		fmt.Printf("Models for %s:\n\n", p.Name)
		printModelReport(available, unavailable)
		fmt.Printf("\nTotal: %d model(s)\n", len(displayModels))

		return nil
	},
}

func init() {
	modelsCmd.Flags().BoolVarP(&modelsShowAll, "all", "a", false, "Show all provider models (not just configured ones)")
	rootCmd.AddCommand(modelsCmd)
}
