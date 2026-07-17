package cmd

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

const (
	autoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	autoCompactPctEnv    = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"

	compactWindow200K = "200000"
	compactWindow500K = "500000"
	compactWindow1M   = "1000000"
	compactPct200K    = "70"
	compactPct500K    = "80"
	compactPct1M      = "90"
)

// compactPreset selects the provider-wide auto-compact budget.
// It is independent of per-slot [1m] extended-context markers: a model may
// support 1M context while compact still triggers earlier (e.g. 200K/70%).
type compactPreset uint8

const (
	// compactPresetPreserve keeps existing compact env values as-is.
	compactPresetPreserve compactPreset = iota
	// compactPreset200K is switch-safe: compact near ~140K.
	compactPreset200K
	// compactPreset500K is balanced for confirmed 1M models: compact near ~400K.
	compactPreset500K
	// compactPreset1M maximizes depth: compact near ~900K.
	compactPreset1M
	// compactPresetDefault removes ccl-managed compact env so Claude Code uses
	// its built-in default auto-compact behaviour (it does NOT disable compact).
	compactPresetDefault
)

type compactConfigState struct {
	preset compactPreset
	legacy bool
	custom bool
	window string
	pct    string
}

func compactStateFromProvider(p provider.Provider) compactConfigState {
	window, pct := "", ""
	if p.Env != nil {
		window = strings.TrimSpace(p.Env[autoCompactWindowEnv])
		pct = strings.TrimSpace(p.Env[autoCompactPctEnv])
	}

	// Compact presets are detected only from env values, never from [1m] slots.
	switch {
	case window == compactWindow1M && pct == compactPct1M:
		return compactConfigState{preset: compactPreset1M, window: window, pct: pct}
	case window == compactWindow1M && pct == "":
		// Legacy: older ccl wrote window=1M without a percentage.
		return compactConfigState{preset: compactPresetPreserve, legacy: true, window: window}
	case window == compactWindow500K && pct == compactPct500K:
		return compactConfigState{preset: compactPreset500K, window: window, pct: pct}
	case window == compactWindow200K && pct == compactPct200K:
		return compactConfigState{preset: compactPreset200K, window: window, pct: pct}
	case window == "" && pct == "":
		return compactConfigState{preset: compactPresetDefault}
	default:
		return compactConfigState{preset: compactPresetPreserve, custom: true, window: window, pct: pct}
	}
}

func compactPresetLabel(preset compactPreset) string {
	switch preset {
	case compactPreset200K:
		return "Switch-safe 200K / 70%"
	case compactPreset500K:
		return "Balanced 500K / 80%"
	case compactPreset1M:
		return "Maximum 1M / 90%"
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
		window, pct := state.window, state.pct
		if window == "" {
			window = "unset"
		}
		if pct == "" {
			pct = "unset"
		} else {
			pct += "%"
		}
		return fmt.Sprintf("custom %s / %s (preserved)", window, pct)
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
// auto-compact budget independently.
func applyCompactConfig(p *provider.Provider, oneMSlots map[string]bool, preset compactPreset) {
	applyOneMSuffixes(p, oneMSlots)

	switch preset {
	case compactPresetPreserve:
		// Keep existing compact env values; only suffixes were normalized.
		return
	case compactPreset1M:
		ensureProviderEnv(p)
		p.Env[autoCompactWindowEnv] = compactWindow1M
		p.Env[autoCompactPctEnv] = compactPct1M
	case compactPreset500K:
		ensureProviderEnv(p)
		p.Env[autoCompactWindowEnv] = compactWindow500K
		p.Env[autoCompactPctEnv] = compactPct500K
	case compactPreset200K:
		ensureProviderEnv(p)
		p.Env[autoCompactWindowEnv] = compactWindow200K
		p.Env[autoCompactPctEnv] = compactPct200K
	case compactPresetDefault:
		if p.Env != nil {
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
// explicit on/off choice for 1M context. Compact budget is only set to 1M/90
// when at least one slot enables extended context; otherwise Claude default.
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
