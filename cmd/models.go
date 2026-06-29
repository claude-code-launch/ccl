package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/protocol"
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

		var modelsStr string
		if p.Model == "" {
			// No configured models, fetch from provider
			if p.Type == "openai" {
				if m, err := protocol.GetOpenAIModels(p.Endpoint, p.APIKey); err != nil {
					return err
				} else {
					modelsStr = m
				}
			} else {
				if m, err := protocol.GetAnthropicModels(p.Endpoint, p.APIKey); err != nil {
					return err
				} else {
					modelsStr = m
				}
			}
		} else {
			modelsStr = p.Model
		}

		modelList := parseModelList(modelsStr)
		if len(modelList) == 0 {
			fmt.Println("No models found.")
			return nil
		}

		// Determine whether to show all or only configured
		displayModels := modelList
		if !modelsShowAll {
			// Default: show configured models only (those in p.Model)
			if p.Model != "" {
				displayModels = parseModelList(p.Model)
			}
		}

		// Test each model concurrently
		availableSet := testModelsConcurrently(displayModels, p.Endpoint, p.APIKey)

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
