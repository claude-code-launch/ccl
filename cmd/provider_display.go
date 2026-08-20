package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func providerAuthLabel(p provider.Provider) string {
	if p.OAuthProvider != "" {
		return "oauth/" + p.OAuthProvider
	}
	if provider.IsOpenAICompatibleType(p.Type) || provider.IsCommandCodeType(p.Type) {
		return "bearer"
	}
	if provider.IsAnthropicType(p.Type) {
		if strings.EqualFold(p.AnthropicAuth, "bearer") {
			return "bearer"
		}
		return "x-api-key"
	}
	return "unknown"
}

func providerEffortSummary(p provider.Provider) string {
	if strings.TrimSpace(p.EffortLevel) == "" {
		return "default"
	}
	return p.EffortLevel
}

// providerFastSummary reports the Codex fastMode state for display. Only Codex
// Responses OAuth backends (gpt) honour it; other providers show
// "off" regardless of the stored flag.
func providerFastSummary(p provider.Provider) string {
	if p.FastMode && (strings.EqualFold(p.OAuthProvider, "gpt") || strings.EqualFold(p.OAuthProvider, "chatgpt")) {
		return "on"
	}
	return "off"
}

func subagentMappingDisplay(p provider.Provider) string {
	return subagentMappingDisplayWithNames(p, nil)
}

func subagentMappingDisplayWithNames(p provider.Provider, names map[string]string) string {
	if model := strings.TrimSpace(p.SubagentModel); model != "" {
		return providerCatalogModelLabel(model, names)
	}
	if model, ok := p.Env[claude.SubagentModelEnv]; ok && strings.TrimSpace(model) != "" {
		return fmt.Sprintf("(env: %s)", providerCatalogModelLabel(strings.TrimSpace(model), names))
	}
	effective := strings.TrimSpace(claude.ResolveRuntimeSettings(p).SubagentModel)
	if effective == "" {
		return "(auto)"
	}
	return fmt.Sprintf("(auto: %s)", providerCatalogModelLabel(effective, names))
}

func providerCatalogModelLabel(model string, names map[string]string) string {
	technical := strings.TrimSpace(model)
	base := stripOneMSuffix(technical)
	if base == "" {
		return technical
	}
	display := ""
	for id, name := range names {
		if strings.EqualFold(strings.TrimSpace(id), base) {
			display = strings.TrimSpace(name)
			break
		}
	}
	if display == "" || strings.EqualFold(display, base) {
		return technical
	}
	label := display + " (" + base + ")"
	if base != technical {
		label += " · 1M"
	}
	return label
}

func providerOneMSummary(p provider.Provider) string {
	contextPart := reviewOneMSummary(oneMSlotsFromProvider(p))
	if provider.IsBalancedContextPreset(p.Env) {
		return "500K/400K · " + contextPart
	}
	return "default (200K/1M) · " + contextPart
}

func setProviderAuthHeaders(req *http.Request, p provider.Provider) {
	if provider.IsOpenAICompatibleType(p.Type) || provider.IsCommandCodeType(p.Type) {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		return
	}
	if strings.EqualFold(p.AnthropicAuth, "bearer") {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	} else {
		req.Header.Set("x-api-key", p.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
}

func printProviderExperienceWarnings(p provider.Provider) {
	if strings.TrimSpace(p.EffortLevel) != "" {
		doctorWarn("Effort is pinned by ccl; choose Default in ccl set if Claude /model effort changes should apply.")
	}
	if p.FastMode {
		doctorWarn("FastMode is on: Codex faster responses at higher usage; toggle with /fast in Claude Code or ccl set Review & Apply.")
	}
	if p.OAuthProvider == "" && provider.IsOpenAICompatibleType(p.Type) && endpointPathIsEmpty(p.Endpoint) {
		doctorWarn("OpenAI-compatible endpoint has no path; if model tests fail, try adding /v1 or re-run ccl set for Anthropic-compatible gateways.")
	}
}

func endpointPathIsEmpty(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	return strings.Trim(u.Path, "/") == ""
}
