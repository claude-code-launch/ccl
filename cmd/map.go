package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/slotrec"
	"github.com/spf13/cobra"

	tui "github.com/grindlemire/go-tui"
)

var mapCmd = newMapCommand("map [provider-name]")

type mapOptions struct {
	opus     string
	sonnet   string
	haiku    string
	fable    string
	custom   string
	subagent string
	probe    bool
}

var fetchMappingCatalog = fetchMappingCatalogFromProvider

func newMapCommand(use string) *cobra.Command {
	opts := &mapOptions{}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Quickly set Claude slot-to-model mappings",
		Long: `Set which provider model each Claude slot uses.

Modes:
  ccl map                        Interactive TUI - enter slot mapping page directly
  ccl map auto                   Auto-fill slots from the catalog (same rules as ccl set)
  ccl map auto --probe           Test each model first (a billed request each)
  ccl map --opus <m> --sonnet <m>  Direct CLI mapping

Examples:
  ccl map
  ccl provider map
  ccl map auto
  ccl provider map auto
  ccl map auto my-provider
  ccl map --opus gpt-5.1 --sonnet gpt-5.1-codex-max
  ccl map --custom gpt-5.1 my-provider
  ccl map --fable gpt-5.6-sol my-provider
  ccl map --subagent gpt-5.4-mini my-provider
  ccl provider map --custom gpt-5.1 my-provider`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hasFlag := cmd.Flags().Changed("opus") || cmd.Flags().Changed("sonnet") ||
				cmd.Flags().Changed("haiku") || cmd.Flags().Changed("fable") ||
				cmd.Flags().Changed("custom") || cmd.Flags().Changed("subagent")

			if hasFlag {
				return runMapDirect(cmd, args, opts)
			}
			if len(args) > 0 && args[0] == "auto" {
				return runMapAuto(cmd.Context(), args[1:], opts.probe)
			}
			return runMapTUI(args)
		},
	}
	cmd.Flags().StringVar(&opts.opus, "opus", "", "Model ID for Opus slot")
	cmd.Flags().StringVar(&opts.sonnet, "sonnet", "", "Model ID for Sonnet slot")
	cmd.Flags().StringVar(&opts.haiku, "haiku", "", "Model ID for Haiku slot")
	cmd.Flags().StringVar(&opts.fable, "fable", "", "Model ID for Fable slot")
	cmd.Flags().StringVar(&opts.custom, "custom", "", "Model ID for Custom slot")
	cmd.Flags().StringVar(&opts.subagent, "subagent", "", "Model ID for Claude Code subagents (empty uses automatic selection)")
	cmd.Flags().BoolVar(&opts.probe, "probe", false, "With auto: test each model with a 1-token request first (billed)")
	return cmd
}

// runMapDirect applies direct slot mapping flags.
func runMapDirect(cmd *cobra.Command, args []string, opts *mapOptions) error {
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
		p.OpusModel = opts.opus
	}
	if cmd.Flags().Changed("sonnet") {
		p.SonnetModel = opts.sonnet
	}
	if cmd.Flags().Changed("haiku") {
		p.HaikuModel = opts.haiku
	}
	if cmd.Flags().Changed("fable") {
		p.FableModel = opts.fable
	}
	if cmd.Flags().Changed("custom") {
		p.CustomModelID = opts.custom
	}
	if cmd.Flags().Changed("subagent") {
		p.SubagentModel = opts.subagent
		if p.Env != nil {
			delete(p.Env, claude.SubagentModelEnv)
		}
	}

	applyOneMSuffixes(&p, oneMSlotsFromProvider(p))

	if err := replaceProvider(providerName, p); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Updated slot mapping for provider %q:\n", providerName)
	if cmd.Flags().Changed("opus") {
		fmt.Printf("  Opus ID     -> %s\n", p.OpusModel)
	}
	if cmd.Flags().Changed("sonnet") {
		fmt.Printf("  Sonnet ID   -> %s\n", p.SonnetModel)
	}
	if cmd.Flags().Changed("haiku") {
		fmt.Printf("  Haiku ID    -> %s\n", p.HaikuModel)
	}
	if cmd.Flags().Changed("fable") {
		fmt.Printf("  Fable ID    -> %s\n", p.FableModel)
	}
	if cmd.Flags().Changed("custom") {
		fmt.Printf("  Custom ID   -> %s\n", p.CustomModelID)
	}
	if cmd.Flags().Changed("subagent") {
		fmt.Printf("  Subagent ID -> %s\n", subagentMappingDisplay(p))
	}

	return nil
}

// runMapAuto fetches available models and maps usable models to slots in order.
func runMapAuto(ctx context.Context, args []string, probe bool) error {
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

	fmt.Printf("Fetching the model catalog for %q...\n", providerName)

	runtimeProvider, models, metadata, cleanup, err := fetchMappingCatalog(ctx, p)
	if err != nil {
		return fmt.Errorf("discover models for provider %q: %w", providerName, err)
	}
	defer cleanup()

	// Recommend from the catalog as is. Probing every model is a billed request
	// each (Copilot premium requests, Qoder credits), so it is opt-in: --probe
	// narrows the pool to the models that actually answered.
	pool := models
	if probe {
		fmt.Printf("Probing %d model(s) with a 1-token request each...\n", len(models))
		availableSet := testModelsConcurrently(ctx, models, runtimeProvider.Endpoint, runtimeProvider.APIKey, probeWireType(runtimeProvider), runtimeProvider.AnthropicAuth, runtimeProvider.ModelProtocols)
		available, unavailable := classifyModels(models, availableSet)
		if len(available) == 0 {
			return fmt.Errorf("no available models found - check endpoint and API key")
		}
		fmt.Printf("Found %d available model(s) out of %d total.\n", len(available), len(models))
		pool = available
		models = append(append([]string(nil), available...), unavailable...)
	}
	if len(pool) == 0 {
		return fmt.Errorf("the provider's catalog is empty")
	}
	p.Model = strings.Join(models, ",")

	// The same recommender as the ccl set page. Mapping starts from scratch, so
	// the current slots are not preserved; a slot keeps its [1m] marker only
	// when the recommender confirms the model serves 1M.
	rec := slotrec.Recommend(provider.Provider{}, pool, metadata)
	p.OpusModel, p.SonnetModel, p.HaikuModel = rec.Opus, rec.Sonnet, rec.Haiku
	p.FableModel, p.CustomModelID, p.SubagentModel = rec.Fable, rec.Custom, rec.Subagent
	applyOneMSuffixes(&p, rec.OneMSlots)
	// Mapping does not change Default/Balanced, but retires unsupported old or
	// hand-written context combinations when encountered.
	if hasUnsupportedContextConfig(p) {
		applyCompactPreset(&p, compactPresetDefault)
	}

	if err := replaceProvider(providerName, p); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n✓ Auto-mapped slots for provider %q:\n", providerName)
	for _, s := range mappedSlotReport(&p) {
		if *s.ptr != "" {
			fmt.Printf("  %-6s -> %s\n", s.name, mappedModelOutputLabel(*s.ptr, metadata))
		} else {
			fmt.Printf("  %-6s -> (unset)\n", s.name)
		}
	}
	if !probe {
		fmt.Println("\nNot probed; `ccl map auto --probe` first tests each model (billed).")
	}
	return nil
}

type modelSlot struct {
	name string
	ptr  *string
}

// mappedSlotReport lists every slot the auto-mapping summary prints, in menu
// order: the four sequential slots plus Fable, which is reported even though it
// is not walked.
func mappedSlotReport(p *provider.Provider) []modelSlot {
	return []modelSlot{
		{"Opus", &p.OpusModel},
		{"Sonnet", &p.SonnetModel},
		{"Haiku", &p.HaikuModel},
		{"Fable", &p.FableModel},
		{"Custom", &p.CustomModelID},
	}
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

	modelPool, metadata, err := modelCatalogForMapping(context.Background(), p)
	if err != nil {
		return fmt.Errorf("discover models for provider %q: %w", providerName, err)
	}

	// Launch TUI at page 1
	m := NewAdvancedMappingModel(&p, modelPool, metadata)
	app, err := tui.NewApp(tui.WithRootComponent(m))
	if err != nil {
		return fmt.Errorf("failed running mapping panel: %w", err)
	}
	defer app.Close()
	if err := app.Run(); err != nil {
		return fmt.Errorf("failed running mapping panel: %w", err)
	}

	updatedModel := m
	p = *updatedModel.p

	applyCompactConfig(&p, updatedModel.live().oneMSlots, updatedModel.live().compactPreset)

	if err := replaceProvider(providerName, p); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n✓ Slot mapping saved for provider %q:\n", providerName)
	printSlot := func(label, val string) {
		if val != "" {
			fmt.Printf("  %-9s -> %s\n", label, mappedModelOutputLabel(val, metadata))
		} else {
			fmt.Printf("  %-9s -> (unset)\n", label)
		}
	}
	printSlot("Opus", p.OpusModel)
	printSlot("Sonnet", p.SonnetModel)
	printSlot("Haiku", p.HaikuModel)
	printSlot("Fable", p.FableModel)
	printSlot("Custom", p.CustomModelID)
	if strings.TrimSpace(p.SubagentModel) != "" {
		printSlot("Subagent", p.SubagentModel)
	} else {
		printSlot("Subagent", subagentMappingDisplay(p))
	}

	return nil
}

func mappedModelOutputLabel(model string, metadata map[string]protocol.ModelInfo) string {
	model = strings.TrimSpace(model)
	base := stripOneMSuffix(model)
	if base == "" || strings.HasPrefix(base, "(") {
		return model
	}
	label := modelReportLabel(base, metadata)
	if base != model {
		label += " · 1M"
	}
	return label
}

func modelCatalogForMapping(ctx context.Context, p provider.Provider) ([]string, map[string]protocol.ModelInfo, error) {
	if configured := parseModelList(p.Model); len(configured) > 0 {
		if strings.TrimSpace(p.OAuthProvider) == "" {
			return configured, nil, nil
		}
	}
	_, models, metadata, cleanup, err := fetchMappingCatalog(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("provider returned no models")
	}
	return models, metadata, nil
}

// fetchMappingCatalogFromProvider returns a provider endpoint suitable for
// live probes plus its authoritative model IDs. OAuth endpoints such as
// oauth://qoder are descriptors rather than HTTP URLs, so they must first be
// resolved to the embedded loopback runtime.
func fetchMappingCatalogFromProvider(ctx context.Context, p provider.Provider) (provider.Provider, []string, map[string]protocol.ModelInfo, func(), error) {
	nop := func() {}
	if strings.TrimSpace(p.OAuthProvider) == "" {
		infos := fetchModelInfosForProvider(p)
		models := modelIDs(infos)
		if len(models) == 0 {
			return p, nil, nil, nop, fmt.Errorf("no models found from provider")
		}
		return p, models, indexModelInfos(infos), nop, nil
	}

	runtimeProvider, runtime, cleanup, err := prepareProviderRuntime(ctx, p)
	if err != nil {
		return provider.Provider{}, nil, nil, nop, err
	}
	infos := fetchModelInfosForProvider(runtimeProvider)
	metadata := indexModelInfos(infos)
	models := runtime.Models()
	if runtime.ModelCatalogIsFallback() {
		// The runtime answered with a compatibility list, not the account's
		// models — usually an expired credential. Mapping against it would offer
		// the user five wrong choices, so prefer the pool already saved.
		if saved := parseModelList(runtimeProvider.Model); len(saved) > 0 {
			models = saved
		}
	}
	if len(models) == 0 {
		models = modelIDs(infos)
	}
	if len(models) == 0 {
		models = parseModelList(runtimeProvider.Model)
	}
	if len(models) == 0 {
		cleanup()
		return provider.Provider{}, nil, nil, nop, fmt.Errorf("OAuth runtime returned no models")
	}
	return runtimeProvider, models, metadata, cleanup, nil
}

// resolveProviderName returns provider name from args or active provider.
func resolveProviderName(args []string, cfg *provider.Config) string {
	if len(args) > 0 {
		return args[0]
	}
	return cfg.ActiveProvider
}

func init() {
	rootCmd.AddCommand(mapCmd)
}
