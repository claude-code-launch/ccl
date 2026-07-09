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

	availableSet := testModelsConcurrently(modelList, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
	available, unavailable := classifyModels(modelList, availableSet)
	fmt.Println()
	printModelReport(available, unavailable)

	return nil
}

func init() {
	rootCmd.AddCommand(modelsCmd)
}
