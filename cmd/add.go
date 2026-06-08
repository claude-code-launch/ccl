package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/haiboyuwen/claude-code-launch/internal/config"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
	"github.com/spf13/cobra"
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new LLM provider config",
	RunE: func(cmd *cobra.Command, args []string) error {
		var p provider.Provider
		var envRaw string
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
				huh.NewText().
					Title("Environment Variables (optional)").
					Description("One KEY=VALUE per line, leave empty to skip").
					Value(&envRaw),
			),
		).Run()

		if err != nil {
			return err
		}
		// 可选，有内容才解析
		for _, line := range strings.Split(envRaw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if p.Env == nil {
				p.Env = make(map[string]string)
			}
			p.Env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}

		// Step 2: Auto-detect Protocol Type
		fmt.Println("\nAuto-detecting provider protocol type...")
		p.Type, p.Model = detectProtocolAndModels(p.Endpoint, p.APIKey)
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

func init() {
	rootCmd.AddCommand(addCmd)
}
