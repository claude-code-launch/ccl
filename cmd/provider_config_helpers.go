package cmd

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

const (
	maxContextTokensEnv  = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	autoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	// autoCompactPctEnv is kept only to recognize and clean up configurations
	// written by ccl versions that used percentage-based compact presets.
	autoCompactPctEnv = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"

	maxContext300K = "300000"
	maxContext500K = "500000"
	maxContext1M   = "1000000"

	compactWindow300K = "200000"
	compactWindow500K = "400000"
	compactWindow1M   = "900000"

	legacyCompactWindow200K = "200000"
	legacyCompactWindow500K = "500000"
	legacyCompactWindow1M   = "1000000"
	legacyCompactPct200K    = "70"
	legacyCompactPct500K    = "80"
	legacyCompactPct1M      = "90"
)

// compactPreset selects the provider-wide fallback context size and absolute
// auto-compact window.
// It is independent of per-slot [1m] extended-context markers: a model may
// support 1M context while an unrecognized model uses a smaller fallback.
type compactPreset uint8

const (
	// compactPresetPreserve keeps existing compact env values as-is.
	compactPresetPreserve compactPreset = iota
	// compactPreset300K is switch-safe: 300K fallback, compact at 200K.
	compactPreset300K
	// compactPreset500K is balanced: 500K fallback, compact at 400K.
	compactPreset500K
	// compactPreset1M maximizes depth: 1M fallback, compact at 900K.
	compactPreset1M
	// compactPresetDefault removes ccl-managed context and compact env so Claude
	// Code uses its built-in defaults (it does NOT disable compact).
	compactPresetDefault
)

type compactConfigState struct {
	preset  compactPreset
	legacy  bool
	custom  bool
	context string
	window  string
	pct     string
}

func compactStateFromProvider(p provider.Provider) compactConfigState {
	contextSize, window, pct := "", "", ""
	if p.Env != nil {
		contextSize = strings.TrimSpace(p.Env[maxContextTokensEnv])
		window = strings.TrimSpace(p.Env[autoCompactWindowEnv])
		pct = strings.TrimSpace(p.Env[autoCompactPctEnv])
	}

	// Presets are detected only from env values, never from [1m] slots.
	// Recognize the old percentage-based pairs as the same preset so the next
	// save transparently migrates them to absolute windows.
	switch {
	case contextSize == maxContext1M && window == compactWindow1M && pct == "":
		return compactConfigState{preset: compactPreset1M, context: contextSize, window: window}
	case contextSize == maxContext500K && window == compactWindow500K && pct == "":
		return compactConfigState{preset: compactPreset500K, context: contextSize, window: window}
	case contextSize == maxContext300K && window == compactWindow300K && pct == "":
		return compactConfigState{preset: compactPreset300K, context: contextSize, window: window}
	case contextSize == "" && window == legacyCompactWindow1M && pct == legacyCompactPct1M:
		return compactConfigState{preset: compactPreset1M, window: window, pct: pct}
	case contextSize == "" && window == legacyCompactWindow500K && pct == legacyCompactPct500K:
		return compactConfigState{preset: compactPreset500K, window: window, pct: pct}
	case contextSize == "" && window == legacyCompactWindow200K && pct == legacyCompactPct200K:
		return compactConfigState{preset: compactPreset300K, window: window, pct: pct}
	case contextSize == "" && window == legacyCompactWindow1M && pct == "":
		// Legacy: older ccl wrote window=1M without a percentage.
		return compactConfigState{preset: compactPresetPreserve, legacy: true, window: window}
	case contextSize == "" && window == "" && pct == "":
		return compactConfigState{preset: compactPresetDefault}
	default:
		return compactConfigState{preset: compactPresetPreserve, custom: true, context: contextSize, window: window, pct: pct}
	}
}

func compactPresetLabel(preset compactPreset) string {
	switch preset {
	case compactPreset300K:
		return "Switch-safe 300K / 200K"
	case compactPreset500K:
		return "Balanced 500K / 400K"
	case compactPreset1M:
		return "Maximum 1M / 900K"
	case compactPresetDefault:
		return "Claude default"
	default:
		return "Custom (preserve)"
	}
}

func compactStateSummary(state compactConfigState, oneMSlots map[string]bool) string {
	if state.legacy {
		return fmt.Sprintf("1M / pct unset (legacy; context %s)", reviewOneMSummary(oneMSlots))
	}
	if state.custom {
		contextSize, window := state.context, state.window
		if contextSize == "" {
			contextSize = "unset"
		}
		if window == "" {
			window = "unset"
		}
		summary := fmt.Sprintf("custom context %s / compact %s", contextSize, window)
		if state.pct != "" {
			summary += " / legacy pct " + state.pct + "%"
		}
		return summary + " (preserved)"
	}
	return compactPresetLabel(state.preset)
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

// applyCompactConfig applies per-slot [1m] markers and the provider-wide
// fallback context/auto-compact preset independently.
func applyCompactConfig(p *provider.Provider, oneMSlots map[string]bool, preset compactPreset) {
	applyOneMSuffixes(p, oneMSlots)

	switch preset {
	case compactPresetPreserve:
		// Keep existing compact env values; only suffixes were normalized.
		return
	case compactPreset1M:
		ensureProviderEnv(p)
		p.Env[maxContextTokensEnv] = maxContext1M
		p.Env[autoCompactWindowEnv] = compactWindow1M
		delete(p.Env, autoCompactPctEnv)
	case compactPreset500K:
		ensureProviderEnv(p)
		p.Env[maxContextTokensEnv] = maxContext500K
		p.Env[autoCompactWindowEnv] = compactWindow500K
		delete(p.Env, autoCompactPctEnv)
	case compactPreset300K:
		ensureProviderEnv(p)
		p.Env[maxContextTokensEnv] = maxContext300K
		p.Env[autoCompactWindowEnv] = compactWindow300K
		delete(p.Env, autoCompactPctEnv)
	case compactPresetDefault:
		if p.Env != nil {
			delete(p.Env, maxContextTokensEnv)
			delete(p.Env, autoCompactWindowEnv)
			delete(p.Env, autoCompactPctEnv)
		}
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

// applyOneMConfig preserves the legacy helper contract for callers that make an
// explicit on/off choice for 1M context. The maximum preset is selected when
// at least one slot enables extended context; otherwise Claude default.
func applyOneMConfig(p *provider.Provider, oneMSlots map[string]bool) {
	if len(oneMSlots) == 0 {
		applyCompactConfig(p, oneMSlots, compactPresetDefault)
		return
	}
	applyCompactConfig(p, oneMSlots, compactPreset1M)
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
