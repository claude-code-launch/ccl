package cmd

import (
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
		Long:  `List all available models for the active provider and test each for availability.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModels(showAll)
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Show all provider models (not just configured ones)")
	return cmd
}

func runModels(showAll bool) error {
	p, err := resolveProvider()
	if err != nil {
		return err
	}

	modelsStr := p.Model
	if showAll || modelsStr == "" {
		fetched := fetchModelsForProvider(p)
		if len(fetched) == 0 {
			if modelsStr == "" || showAll {
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

	availableSet := testModelsConcurrently(displayModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
	available, unavailable := classifyModels(displayModels, availableSet)
	fmt.Printf("Models for %s:\n\n", p.Name)
	printModelReport(available, unavailable)
	fmt.Printf("\nTotal: %d model(s)\n", len(displayModels))

	return nil
}

func init() {
	rootCmd.AddCommand(modelsCmd)
}
