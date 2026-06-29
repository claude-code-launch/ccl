package cmd

import (
	"fmt"
	"sort"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/spf13/cobra"

	tea "charm.land/bubbletea/v2"
)

var (
	mapOpus   string
	mapSonnet string
	mapHaiku  string
	mapCustom string
)

var mapCmd = &cobra.Command{
	Use:   "map [provider-name]",
	Short: "Quickly set Claude slot-to-model mappings",
	Long: `Set which provider model each Claude slot uses.

Modes:
  ccl map                        Interactive TUI - enter slot mapping page directly
  ccl map auto                   Auto-fill slots with best available models
  ccl map --opus <m> --sonnet <m>  Direct CLI mapping

Examples:
  ccl map
  ccl map auto
  ccl map auto my-provider
  ccl map --opus gpt-5.1 --sonnet gpt-5.1-codex-max
  ccl map --custom gpt-5.1 my-provider`,
	RunE: func(cmd *cobra.Command, args []string) error {
		hasFlag := cmd.Flags().Changed("opus") || cmd.Flags().Changed("sonnet") ||
			cmd.Flags().Changed("haiku") || cmd.Flags().Changed("custom")

		if hasFlag {
			return runMapDirect(cmd, args)
		}
		if len(args) > 0 && args[0] == "auto" {
			return runMapAuto(args[1:])
		}
		return runMapTUI(args)
	},
}

// runMapDirect applies --opus/--sonnet/--haiku/--custom flags.
func runMapDirect(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	providerName := resolveProviderName(args, cfg)
	if providerName == "" {
		return fmt.Errorf("no provider specified and no active provider set")
	}

	p, ok := cfg.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider %q not found", providerName)
	}

	if cmd.Flags().Changed("opus") {
		p.OpusModel = mapOpus
	}
	if cmd.Flags().Changed("sonnet") {
		p.SonnetModel = mapSonnet
	}
	if cmd.Flags().Changed("haiku") {
		p.HaikuModel = mapHaiku
	}
	if cmd.Flags().Changed("custom") {
		p.LockModel = mapCustom
	}

	cfg.Providers[providerName] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Updated slot mapping for provider %q:\n", providerName)
	if cmd.Flags().Changed("opus") {
		fmt.Printf("  Opus   -> %s\n", p.OpusModel)
	}
	if cmd.Flags().Changed("sonnet") {
		fmt.Printf("  Sonnet -> %s\n", p.SonnetModel)
	}
	if cmd.Flags().Changed("haiku") {
		fmt.Printf("  Haiku  -> %s\n", p.HaikuModel)
	}
	if cmd.Flags().Changed("custom") {
		fmt.Printf("  Custom -> %s\n", p.LockModel)
	}

	return nil
}

// runMapAuto fetches available models and auto-fills the first 4 slots.
func runMapAuto(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	providerName := resolveProviderName(args, cfg)
	if providerName == "" {
		return fmt.Errorf("no provider specified and no active provider set")
	}

	p, ok := cfg.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider %q not found", providerName)
	}

	fmt.Printf("Fetching available models for %q...\n", providerName)

	models := fetchModelsForProvider(p)
	if len(models) == 0 {
		return fmt.Errorf("no models found from provider")
	}

	availableSet := testModelsConcurrently(models, p.Endpoint, p.APIKey)

	var available []string
	for _, m := range models {
		if availableSet[m] {
			available = append(available, m)
		}
	}

	if len(available) == 0 {
		return fmt.Errorf("no available models found - check endpoint and API key")
	}

	sort.Strings(available)
	fmt.Printf("Found %d available model(s) out of %d total.\n", len(available), len(models))

	slots := []struct {
		name string
		ptr  *string
	}{
		{"Opus", &p.OpusModel},
		{"Sonnet", &p.SonnetModel},
		{"Haiku", &p.HaikuModel},
		{"Custom", &p.LockModel},
	}

	assigned := 0
	for i, s := range slots {
		if i < len(available) {
			*s.ptr = available[i]
			assigned++
		}
	}

	if assigned < 4 {
		fmt.Printf("⚠ Only %d model(s) available, assigned to first %d slot(s).\n", assigned, assigned)
		fmt.Println("   Use 'ccl map' to manually configure remaining slots.")
	}

	cfg.Providers[providerName] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n✓ Auto-mapped slots for provider %q:\n", providerName)
	for i, s := range slots {
		if i < len(available) {
			fmt.Printf("  %-6s -> %s\n", s.name, *s.ptr)
		} else {
			fmt.Printf("  %-6s -> (unset)\n", s.name)
		}
	}

	return nil
}

// runMapTUI launches the interactive TUI at page 1 (slot mapping).
func runMapTUI(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	providerName := resolveProviderName(args, cfg)
	if providerName == "" {
		return fmt.Errorf("no provider specified and no active provider set")
	}

	p, ok := cfg.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider %q not found", providerName)
	}

	// Pre-fetch model pool
	modelPool := parseModelList(p.Model)
	if len(modelPool) == 0 {
		modelPool = fetchModelsForProvider(p)
	}
	if len(modelPool) == 0 {
		return fmt.Errorf("no models available - configure models first with 'ccl set'")
	}

	// Launch TUI at page 1
	m := NewAdvancedConfigModelAtPage1(&p, modelPool)
	program := tea.NewProgram(m)
	finalModel, err := program.Run()
	if err != nil {
		return fmt.Errorf("failed running mapping panel: %w", err)
	}

	updatedModel := finalModel.(*AdvancedConfigModel)
	p = *updatedModel.p

	// Apply 1M suffix from toggles
	apply1MSuffix := func(slotName string, ptr *string) {
		if ptr == nil || *ptr == "" {
			return
		}
		if updatedModel.oneMSlots[slotName] {
			*ptr = *ptr + "[1m]"
		}
	}
	apply1MSuffix("opus", &p.OpusModel)
	apply1MSuffix("sonnet", &p.SonnetModel)
	apply1MSuffix("haiku", &p.HaikuModel)
	apply1MSuffix("custom", &p.LockModel)

	cfg.Providers[providerName] = p
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n✓ Slot mapping saved for provider %q:\n", providerName)
	printSlot := func(label, val string) {
		if val != "" {
			fmt.Printf("  %-6s -> %s\n", label, val)
		} else {
			fmt.Printf("  %-6s -> (unset)\n", label)
		}
	}
	printSlot("Opus", p.OpusModel)
	printSlot("Sonnet", p.SonnetModel)
	printSlot("Haiku", p.HaikuModel)
	printSlot("Custom", p.LockModel)

	return nil
}

// resolveProviderName returns provider name from args or active provider.
func resolveProviderName(args []string, cfg *provider.Config) string {
	if len(args) > 0 {
		return args[0]
	}
	return cfg.ActiveProvider
}

// fetchModelsForProvider fetches models from the provider API.
func fetchModelsForProvider(p provider.Provider) []string {
	var modelsStr string
	var err error
	if p.Type == "openai" {
		modelsStr, err = protocol.GetOpenAIModels(p.Endpoint, p.APIKey)
	} else {
		modelsStr, err = protocol.GetAnthropicModels(p.Endpoint, p.APIKey)
	}
	if err != nil || modelsStr == "" {
		return nil
	}
	return parseModelList(modelsStr)
}

func init() {
	mapCmd.Flags().StringVar(&mapOpus, "opus", "", "Model for Opus slot")
	mapCmd.Flags().StringVar(&mapSonnet, "sonnet", "", "Model for Sonnet slot")
	mapCmd.Flags().StringVar(&mapHaiku, "haiku", "", "Model for Haiku slot")
	mapCmd.Flags().StringVar(&mapCustom, "custom", "", "Model for Custom/Lock slot")
	rootCmd.AddCommand(mapCmd)
}
