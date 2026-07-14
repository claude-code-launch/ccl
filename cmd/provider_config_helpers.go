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
	compactWindow1M   = "1000000"
	compactPct200K    = "70"
	compactPct1M      = "90"
)

type compactPreset uint8

const (
	compactPresetPreserve compactPreset = iota
	compactPreset200K
	compactPreset1M
	compactPresetOff
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
	has1M := len(oneMSlotsFromProvider(p)) > 0

	switch {
	case has1M && window == compactWindow1M && pct == compactPct1M:
		return compactConfigState{preset: compactPreset1M, window: window, pct: pct}
	case has1M && window == compactWindow1M && pct == "":
		return compactConfigState{preset: compactPresetPreserve, legacy: true, window: window}
	case !has1M && window == compactWindow200K && pct == compactPct200K:
		return compactConfigState{preset: compactPreset200K, window: window, pct: pct}
	case !has1M && window == "" && pct == "":
		return compactConfigState{preset: compactPresetOff}
	default:
		return compactConfigState{preset: compactPresetPreserve, custom: true, window: window, pct: pct}
	}
}

func compactPresetLabel(preset compactPreset) string {
	switch preset {
	case compactPreset200K:
		return "Confirmed 200K / 70%"
	case compactPreset1M:
		return "1M / 90%"
	case compactPresetOff:
		return "Off"
	default:
		return "Preserve existing/custom"
	}
}

func compactStateSummary(state compactConfigState, oneMSlots map[string]bool) string {
	if state.legacy {
		return fmt.Sprintf("1M / pct unset (legacy; %s)", reviewOneMSummary(oneMSlots))
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
	if state.preset == compactPreset1M {
		return fmt.Sprintf("1M / 90%% (%s)", reviewOneMSummary(oneMSlots))
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

func applyCompactConfig(p *provider.Provider, oneMSlots map[string]bool, preset compactPreset) {
	applySuffixes := func(enabled bool) {
		for _, slot := range advancedSlotRefs(p) {
			*slot.ptr = stripOneMSuffix(*slot.ptr)
			if enabled && *slot.ptr != "" && oneMSlots[slot.key] {
				*slot.ptr += "[1m]"
			}
		}
	}

	switch preset {
	case compactPresetPreserve:
		// Mapping commands use preserve mode so custom provider-level settings are
		// never silently rewritten. Normalize only suffixes already selected.
		applySuffixes(true)
		return
	case compactPreset1M:
		applySuffixes(true)
		ensureProviderEnv(p)
		p.Env[autoCompactWindowEnv] = compactWindow1M
		p.Env[autoCompactPctEnv] = compactPct1M
	case compactPreset200K:
		applySuffixes(false)
		ensureProviderEnv(p)
		p.Env[autoCompactWindowEnv] = compactWindow200K
		p.Env[autoCompactPctEnv] = compactPct200K
	case compactPresetOff:
		applySuffixes(false)
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
// explicit on/off choice. New UI paths should call applyCompactConfig directly.
func applyOneMConfig(p *provider.Provider, oneMSlots map[string]bool) {
	if len(oneMSlots) == 0 {
		applyCompactConfig(p, oneMSlots, compactPresetOff)
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
