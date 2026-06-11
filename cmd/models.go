package cmd

import (
	"fmt"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List available models with metadata",
	Long:  `List all available models for the active provider, including context window, max output tokens, tier, and capabilities.`,
	RunE: func(cmd *cobra.Command, args []string) error {

		p, err := resolveProvider()
		if err != nil {
			return err
		}
		if p.Model == "" {
			if p.Type == "openai" {
				if m, err := protocol.GetOpenAIModels(p.Endpoint, p.APIKey); err != nil {
					return err
				} else {
					p.Model = m
				}
			} else {
				if m, err := protocol.GetAnthropicModels(p.Endpoint, p.APIKey); err != nil {
					return err
				} else {
					p.Model = m
				}
			}
		}

		fmt.Println(p.Model)
		return nil

	},
}

func init() {
	rootCmd.AddCommand(modelsCmd)
}
