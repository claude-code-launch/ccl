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
	if provider.IsOpenAICompatibleType(p.Type) {
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
// Responses OAuth backends (chatgpt/copilot) honour it; other providers show
// "off" regardless of the stored flag.
func providerFastSummary(p provider.Provider) string {
	if p.FastMode && (strings.EqualFold(p.OAuthProvider, "chatgpt") || strings.EqualFold(p.OAuthProvider, "copilot")) {
		return "on"
	}
	return "off"
}

func subagentMappingDisplay(p provider.Provider) string {
	if model := strings.TrimSpace(p.SubagentModel); model != "" {
		return model
	}
	if model, ok := p.Env[claude.SubagentModelEnv]; ok && strings.TrimSpace(model) != "" {
		return fmt.Sprintf("(env: %s)", strings.TrimSpace(model))
	}
	effective := strings.TrimSpace(claude.ResolveRuntimeSettings(p).SubagentModel)
	if effective == "" {
		return "(auto)"
	}
	return fmt.Sprintf("(auto: %s)", effective)
}

func providerOneMSummary(p provider.Provider) string {
	state := compactStateFromProvider(p)
	slots := oneMSlotsFromProvider(p)
	contextPart := reviewOneMSummary(slots)
	if state.legacy {
		return "legacy 1M · " + contextPart
	}
	if state.custom {
		return "custom · " + contextPart
	}
	switch state.preset {
	case compactPreset1M:
		return "1M/900K · " + contextPart
	case compactPreset500K:
		return "500K/400K · " + contextPart
	case compactPreset300K:
		return "300K/200K · " + contextPart
	case compactPresetDefault:
		return "default · " + contextPart
	default:
		return "custom · " + contextPart
	}
}

func setProviderAuthHeaders(req *http.Request, p provider.Provider) {
	if provider.IsOpenAICompatibleType(p.Type) {
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
		fmt.Println("  ! Effort is pinned by ccl; choose Default in ccl set if Claude /model effort changes should apply.")
	}
	if p.FastMode {
		fmt.Println("  ! FastMode is on: Codex faster responses at higher usage; toggle with /fast in Claude Code or ccl set Review & Apply.")
	}
	if p.OAuthProvider == "" && provider.IsOpenAICompatibleType(p.Type) && endpointPathIsEmpty(p.Endpoint) {
		fmt.Println("  ! OpenAI-compatible endpoint has no path; if model tests fail, try adding /v1 or re-run ccl set for Anthropic-compatible gateways.")
	}
}

func endpointPathIsEmpty(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	return strings.Trim(u.Path, "/") == ""
}
