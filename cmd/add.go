package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/haiboyuwen/claude-code-launch/internal/config"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new LLM provider config",
	RunE: func(cmd *cobra.Command, args []string) error {
		var p provider.Provider

		// Step 1: Gather general metadata
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Provider Name").
					Description("A unique identifier (e.g. openrouter, deepseek, company)").
					Value(&p.Name).
					Validate(func(str string) error {
						if str == "" {
							return errors.New("provider name cannot be empty")
						}
						return nil
					}),

				huh.NewInput().
					Title("Endpoint (URL)").
					Description("The base API endpoint URL (e.g., https://api.deepseek.com/v1)").
					Value(&p.Endpoint).
					Validate(func(str string) error {
						if str == "" {
							return errors.New("endpoint cannot be empty")
						}
						return nil
					}),

				huh.NewInput().
					Title("API Key").
					Description("Your API key for this provider").
					Value(&p.APIKey).
					Validate(func(str string) error {
						if str == "" {
							return errors.New("API Key cannot be empty")
						}
						return nil
					}),

				huh.NewInput().
					Title("Model").
					Description("The model identifier (e.g., deepseek-chat, gpt-4o, claude-3-5-sonnet)").
					Value(&p.Model).
					Validate(func(str string) error {
						if str == "" {
							return errors.New("model name cannot be empty")
						}
						return nil
					}),
			),
		).Run()

		if err != nil {
			return err
		}

		// Step 2: Auto-detect Protocol Type
		fmt.Println("\nAuto-detecting provider protocol type...")
		p.Type = detectProtocol(p.Endpoint, p.APIKey)
		fmt.Printf("✓ Detected Protocol Type: %s\n\n", p.Type)

		// Confirm or let user overwrite
		var confirmType bool = true
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Register as %s compatible?", strings.ToUpper(p.Type))).
					Description("Is this correct?").
					Value(&confirmType),
			),
		).Run()

		if err != nil {
			return err
		}

		if !confirmType {
			err = huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Force Provider Type").
						Options(
							huh.NewOption("OpenAI Compatible", "openai"),
							huh.NewOption("Anthropic Native", "anthropic"),
						).
						Value(&p.Type),
				),
			).Run()
			if err != nil {
				return err
			}
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if cfg.Providers == nil {
			cfg.Providers = make(map[string]provider.Provider)
		}

		cfg.Providers[p.Name] = p

		// Automatically activate if it is the first/only provider
		if cfg.ActiveProvider == "" {
			cfg.ActiveProvider = p.Name
		}

		err = config.Save(cfg)
		if err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("Successfully added provider %q!\n", p.Name)
		if cfg.ActiveProvider == p.Name {
			fmt.Printf("Activated provider %q.\n", p.Name)
		}
		return nil
	},
}

// detectProtocol probes the endpoint to figure out if it is an openai or anthropic endpoint.
func detectProtocol(endpoint, apiKey string) string {
	endpoint = strings.TrimSuffix(endpoint, "/")

	// We'll run a quick models probe first which is standard for OpenAI
	modelsURL := endpoint + "/models"
	if !strings.HasSuffix(endpoint, "/v1") && !strings.HasSuffix(endpoint, "/v1/chat/completions") {
		// Try fallback models endpoint format
		modelsURL = endpoint + "/v1/models"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				// If /models returns OK (usually OpenAI lists models here), it is OpenAI
				return "openai"
			}
		}
	}

	// Default heuristical probe based on URL patterns
	if strings.Contains(endpoint, "anthropic.com") {
		return "anthropic"
	}

	// Default fallback to openai compatible
	return "openai"
}

func init() {
	rootCmd.AddCommand(addCmd)
}
