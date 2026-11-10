package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func providerAuthLabel(p provider.Provider) string {
	if p.OAuthProvider != "" {
		return "oauth/" + p.OAuthProvider
	}
	if provider.IsOpenAICompatibleType(p.Type) || provider.IsModelsDevType(p.Type) {
		return "bearer"
	}
	return "unknown"
}

func providerEffortSummary(p provider.Provider) string {
	summary := "default"
	if level := strings.TrimSpace(p.EffortLevel); level != "" {
		summary = level
	}
	return summary
}

// providerFastSummary reports the Codex fastMode state for display. Only Codex
// Responses OAuth backends (gpt) honour it; other providers show
// "off" regardless of the stored flag.
func providerFastSummary(p provider.Provider) string {
	if p.FastMode && strings.EqualFold(p.OAuthProvider, "gpt") {
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
	return "(auto)"
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
	switch provider.ProviderContextPreset(p) {
	case provider.ContextPresetBalanced500K:
		return "500K/425K · " + contextPart
	case provider.ContextPresetBalanced800K:
		return "800K/680K · " + contextPart
	default:
		return "default (200K/1M) · " + contextPart
	}
}

func setProviderAuthHeaders(req *http.Request, p provider.Provider) {
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
}

func printProviderExperienceWarnings(p provider.Provider) {
	if level := strings.TrimSpace(p.EffortLevel); level != "" {
		doctorInfo(fmt.Sprintf("Effort starts at %s each session (ccl provider effort default leaves it to Grok Build).", level))
	}
	if p.FastMode {
		doctorInfo("Fast is on: ccl asks the Codex upstream for priority service (faster, higher usage); turn it off in ccl set Review & Apply.")
	}
	if p.OAuthProvider == "" && provider.IsOpenAICompatibleType(p.Type) && endpointPathIsEmpty(p.Endpoint) {
		doctorWarn("OpenAI-compatible endpoint has no path; if model tests fail, try adding /v1.")
	}
}

func endpointPathIsEmpty(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	return strings.Trim(u.Path, "/") == ""
}
