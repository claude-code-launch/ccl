package cmd

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

const (
	maxContextTokensEnv  = provider.EnvMaxContextTokens
	autoCompactWindowEnv = provider.EnvAutoCompactWindow
	autoCompactPctEnv    = provider.EnvAutoCompactPct
)

// compactPreset selects the provider-wide context behavior exposed by the TUI.
type compactPreset = provider.ContextPreset

const (
	compactPresetDefault      = provider.ContextPresetDefault
	compactPresetBalanced500K = provider.ContextPresetBalanced500K
	compactPresetBalanced800K = provider.ContextPresetBalanced800K
	// compactPresetBalanced keeps existing call sites and tests pinned to the
	// original 500K Balanced tier.
	compactPresetBalanced = compactPresetBalanced500K
)

func compactPresetFromProvider(p provider.Provider) compactPreset {
	return provider.ContextPresetFromEnv(p.Env)
}

func hasUnsupportedContextConfig(p provider.Provider) bool {
	return provider.HasManagedContextEnv(p.Env) && provider.ContextPresetFromEnv(p.Env) == provider.ContextPresetDefault
}

func compactPresetLabel(preset compactPreset) string {
	switch preset {
	case compactPresetBalanced500K:
		return "Balanced 500K / 1M & 80%"
	case compactPresetBalanced800K:
		return "Balanced 800K / 1M & 80%"
	default:
		return "Default  200K / 1M & 80%"
	}
}

func recommendedOneMModel(model string) bool {
	switch strings.ToLower(stripOneMSuffix(model)) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	default:
		return false
	}
}

func allConfiguredModelsRecommendOneM(p provider.Provider) bool {
	found := false
	for _, slot := range advancedSlotRefs(&p) {
		model := strings.TrimSpace(*slot.ptr)
		if model == "" {
			continue
		}
		found = true
		if !recommendedOneMModel(model) {
			return false
		}
	}
	return found
}

// applyOneMSuffixes writes or clears the [1m] marker on each configured slot.
// Compact presets never call this with a forced false — only the user's
// Extended Context checkboxes drive the markers.
func applyOneMSuffixes(p *provider.Provider, oneMSlots map[string]bool) {
	for _, slot := range advancedSlotRefs(p) {
		*slot.ptr = stripOneMSuffix(*slot.ptr)
		if *slot.ptr != "" && oneMSlots[slot.key] {
			*slot.ptr += "[1m]"
		}
	}
}

// applyCompactConfig applies the per-slot [1m] markers and the context sizing
// choice.
//
// Default clears every context override so Claude Code uses its native 200K/1M
// behavior. Balanced writes the exact triplet selected by the UI.
func applyCompactConfig(p *provider.Provider, oneMSlots map[string]bool, preset compactPreset) {
	applyOneMSuffixes(p, oneMSlots)
	applyCompactPreset(p, preset)
}

func applyCompactPreset(p *provider.Provider, preset compactPreset) {
	if p.Env != nil {
		delete(p.Env, maxContextTokensEnv)
		delete(p.Env, autoCompactWindowEnv)
		delete(p.Env, autoCompactPctEnv)
		delete(p.Env, provider.EnvContextBudgetMode)
	}
	if maxContext, compactWindow, compactPct, ok := provider.ContextPresetValues(preset); ok {
		ensureProviderEnv(p)
		p.Env[maxContextTokensEnv] = maxContext
		p.Env[autoCompactWindowEnv] = compactWindow
		p.Env[autoCompactPctEnv] = compactPct
	}
	if len(p.Env) == 0 {
		p.Env = nil
	}
}

func ensureProviderEnv(p *provider.Provider) {
	if p.Env == nil {
		p.Env = make(map[string]string)
	}
}

func oneMSlotsFromProvider(p provider.Provider) map[string]bool {
	slots := make(map[string]bool)
	for _, slot := range []struct {
		name  string
		model string
	}{
		{"opus", p.OpusModel},
		{"sonnet", p.SonnetModel},
		{"haiku", p.HaikuModel},
		{"custom", p.CustomModelID},
		{"subagent", p.SubagentModel},
	} {
		if hasOneMSuffix(slot.model) {
			slots[slot.name] = true
		}
	}
	return slots
}

func stripOneMSuffix(model string) string {
	model = strings.TrimSpace(model)
	for strings.HasSuffix(model, "[1m]") {
		model = strings.TrimSpace(strings.TrimSuffix(model, "[1m]"))
	}
	return model
}

func hasOneMSuffix(model string) bool {
	return strings.HasSuffix(strings.TrimSpace(model), "[1m]")
}

// modelDisplayName is the human-facing label for Claude Code *_NAME env vars.
// The technical model ID may keep the [1m] suffix; the display name uses (1M).
func modelDisplayName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if hasOneMSuffix(model) {
		return stripOneMSuffix(model) + " (1M)"
	}
	return model
}
