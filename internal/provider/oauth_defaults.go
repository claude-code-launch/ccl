package provider

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

// PreferredOAuthSlotDefaults returns the first-choice Claude slot mapping for a
// subscription OAuth backend. ok is false when the backend has no built-in
// preferences and should rely entirely on runtime model discovery.
func PreferredOAuthSlotDefaults(oauthProvider string) (custom, opus, sonnet, haiku string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "gpt", "chatgpt":
		// GPT / Codex subscription defaults. Runtime validation drops any of these
		// that are missing from the live /models list so auto-discovery can fill in.
		// "chatgpt" is accepted for older oauthProvider values.
		return "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", true
	case "grok":
		// Grok subscription defaults (xAI). Runtime validation drops any of these
		// that are missing from the live /models list so auto-discovery can fill in.
		return "grok-4.5", "grok-4.5", "grok-4.3", "grok-3-mini", true
	case "gemini":
		// Gemini / Antigravity subscription defaults. Same missing-catalog fallback.
		return "claude-opus-4-6-thinking", "claude-opus-4-6-thinking", "claude-sonnet-4-6", "gemini-3.1-pro-low", true
	default:
		return "", "", "", "", false
	}
}

// ApplyOAuthSlotDefaults fills empty Custom/Opus/Sonnet/Haiku slots with the
// preferred defaults for p.OAuthProvider. Existing user mappings are preserved.
func ApplyOAuthSlotDefaults(p *Provider) {
	if p == nil {
		return
	}
	custom, opus, sonnet, haiku, ok := PreferredOAuthSlotDefaults(p.OAuthProvider)
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
	custom, opus, sonnet, haiku, ok := PreferredOAuthSlotDefaults(p.OAuthProvider)
	if !ok || len(availableModels) == 0 {
		return
	}
	if isPreferredDefault(p.CustomModelID, custom) && !modelListContains(availableModels, p.CustomModelID) {
		p.CustomModelID = ""
	}
	if isPreferredDefault(p.OpusModel, opus) && !modelListContains(availableModels, p.OpusModel) {
		p.OpusModel = ""
	}
	if isPreferredDefault(p.SonnetModel, sonnet) && !modelListContains(availableModels, p.SonnetModel) {
		p.SonnetModel = ""
	}
	if isPreferredDefault(p.HaikuModel, haiku) && !modelListContains(availableModels, p.HaikuModel) {
		p.HaikuModel = ""
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

// stripContextSuffix removes display-only context markers such as [1m] so
// preferred IDs match catalog entries that omit the suffix.
func stripContextSuffix(model string) string {
	base := strings.TrimSpace(model)
	for strings.HasSuffix(base, "[1m]") {
		base = strings.TrimSpace(strings.TrimSuffix(base, "[1m]"))
	}
	return base
}
