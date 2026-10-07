package provider

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

// PreferredOAuthSlotDefaults returns the first-choice Claude slot mapping for a
// subscription OAuth backend. ok is false when the backend has no built-in
// preferences and should rely entirely on runtime model discovery.
//
// fable mirrors opus: no subscription catalog seen so far offers a Fable-family
// model, so the tier follows the backend's strongest model instead of inventing
// an upstream ID that would fail discovery.
func PreferredOAuthSlotDefaults(oauthProvider string) (custom, opus, sonnet, haiku, fable string, ok bool) {
	custom, opus, sonnet, haiku, ok = preferredOAuthSlots(oauthProvider)
	return custom, opus, sonnet, haiku, opus, ok
}

func preferredOAuthSlots(oauthProvider string) (custom, opus, sonnet, haiku string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "gpt":
		// GPT / Codex subscription defaults. Runtime validation drops any of these
		// that are missing from the live /models list so auto-discovery can fill in.
		return "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", true
	case "grok":
		// Grok subscription defaults (xAI). Runtime validation drops any of these
		// that are missing from the live /models list so auto-discovery can fill in.
		return "grok-4.6", "grok-4.6", "grok-4.5", "grok-4.5", true
	case "gemini":
		// Gemini / Antigravity subscription defaults. Same missing-catalog fallback.
		return "claude-opus-4-6-thinking", "claude-opus-4-6-thinking", "claude-sonnet-4-6", "gemini-3.1-pro-low", true
	case "kimi":
		// Kimi Code has no public model-list endpoint, so there is nothing for
		// runtime discovery to fall back on: without these the runtime refuses to
		// start. The IDs are the canonical upstream names from kimi_runtime.go's
		// normalization pass (legacy k2.7-code aliases resolve to the same two).
		return "kimi-for-coding", "kimi-for-coding", "kimi-for-coding", "kimi-for-coding-highspeed", true
	case "kiro":
		// The direct Kiro Messages adapter maps Claude IDs to Amazon Q model IDs.
		return "claude-opus-4-6", "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5", true
	case "autoclaw":
		// AutoClaw's managed zai catalog uses provider-prefixed route IDs. CCL
		// removes that prefix only in the upstream JSON body; the route ID itself
		// stays in X-Request-Model.
		return "zai_auto", "zai_auto", "zaicoding_glm-5.3", "zai_glm-5.3-flash", true
	default:
		return "", "", "", "", false
	}
}

// ApplyOAuthSlotDefaults fills empty Custom/Opus/Sonnet/Haiku/Fable slots with
// the preferred defaults for p.OAuthProvider. Existing user mappings are
// preserved.
func ApplyOAuthSlotDefaults(p *Provider) {
	if p == nil {
		return
	}
	custom, opus, sonnet, haiku, fable, ok := PreferredOAuthSlotDefaults(p.OAuthProvider)
	if !ok {
		return
	}
	if strings.TrimSpace(p.CustomModelID) == "" {
		p.CustomModelID = custom
	}
	if strings.TrimSpace(p.OpusModel) == "" {
		p.OpusModel = opus
	}
	if strings.TrimSpace(p.SonnetModel) == "" {
		p.SonnetModel = sonnet
	}
	if strings.TrimSpace(p.HaikuModel) == "" {
		p.HaikuModel = haiku
	}
	if strings.TrimSpace(p.FableModel) == "" {
		p.FableModel = fable
	}
}

// ClearUnavailablePreferredDefaults removes preferred-default slot mappings that
// are absent from availableModels so the launcher can fall back to auto-discovery
// for those tiers. Non-preferred (user-customized) values are left untouched.
// availableModels is typically the live OAuth /models list; empty is a no-op.
// Mutates p in memory only — does not rewrite config.
func ClearUnavailablePreferredDefaults(p *Provider, availableModels []string) {
	if p == nil {
		return
	}
	custom, opus, sonnet, haiku, fable, ok := PreferredOAuthSlotDefaults(p.OAuthProvider)
	if !ok || len(availableModels) == 0 {
		return
	}
	reconcileOAuthDefault(&p.CustomModelID, custom, availableModels)
	reconcileOAuthDefault(&p.OpusModel, opus, availableModels)
	reconcileOAuthDefault(&p.SonnetModel, sonnet, availableModels)
	reconcileOAuthDefault(&p.FableModel, fable, availableModels)
	reconcileOAuthDefault(&p.HaikuModel, haiku, availableModels)
}

// reconcileOAuthDefault clears a current default that the live catalog no
// longer offers, so discovery can fill the slot. Any other value is the user's
// choice and is left alone. (Values an older ccl generated are migrated once by
// NormalizeProvider, not on every launch.)
func reconcileOAuthDefault(configured *string, preferred string, availableModels []string) {
	if configured == nil || !isPreferredDefault(*configured, preferred) {
		return
	}
	if !modelListContains(availableModels, *configured) {
		*configured = ""
	}
}

func isPreferredDefault(configured, preferred string) bool {
	configured = strings.TrimSpace(configured)
	preferred = strings.TrimSpace(preferred)
	if configured == "" || preferred == "" {
		return false
	}
	return strings.EqualFold(stripContextSuffix(configured), stripContextSuffix(preferred))
}

func modelListContains(availableModels []string, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	want := strings.ToLower(stripContextSuffix(model))
	for _, candidate := range availableModels {
		if strings.EqualFold(stripContextSuffix(candidate), want) {
			return true
		}
	}
	// Also accept exact CSV pool entries that modelrouting may have normalized.
	for _, candidate := range modelrouting.SplitCSV(strings.Join(availableModels, ",")) {
		if strings.EqualFold(stripContextSuffix(candidate), want) {
			return true
		}
	}
	return false
}

// SlotModel pairs a Claude Code model slot with the model mapped to it.
type SlotModel struct {
	Slot  string
	Model string
}

// SlotModels lists the models mapped to Claude Code's slots, in menu order,
// skipping empty slots and stripping display-only markers such as [1m].
func SlotModels(p Provider) []SlotModel {
	candidates := []SlotModel{
		{Slot: "opus", Model: p.OpusModel},
		{Slot: "sonnet", Model: p.SonnetModel},
		{Slot: "haiku", Model: p.HaikuModel},
		{Slot: "fable", Model: p.FableModel},
		{Slot: "custom", Model: p.CustomModelID},
		{Slot: "subagent", Model: p.SubagentModel},
	}
	mapped := make([]SlotModel, 0, len(candidates))
	for _, candidate := range candidates {
		model := stripContextSuffix(candidate.Model)
		if model == "" {
			continue
		}
		mapped = append(mapped, SlotModel{Slot: candidate.Slot, Model: model})
	}
	return mapped
}

// stripContextSuffix removes display-only context markers such as [1m] so
// preferred IDs match catalog entries that omit the suffix.
func stripContextSuffix(model string) string {
	base := strings.TrimSpace(model)
	for strings.HasSuffix(base, "[1m]") {
		base = strings.TrimSpace(strings.TrimSuffix(base, "[1m]"))
	}
	return base
}
