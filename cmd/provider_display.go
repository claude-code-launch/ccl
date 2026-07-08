package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func providerAuthLabel(p provider.Provider) string {
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

func providerOneMSummary(p provider.Provider) string {
	var slots []string
	for _, slot := range []struct {
		name  string
		model string
	}{
		{"opus", p.OpusModel},
		{"sonnet", p.SonnetModel},
		{"haiku", p.HaikuModel},
		{"custom", p.CustomModelID},
	} {
		if hasOneMSuffix(slot.model) {
			slots = append(slots, slot.name)
		}
	}
	if len(slots) == 0 && p.Env != nil && p.Env[autoCompactWindowEnv] == "1000000" {
		return "enabled"
	}
	if len(slots) == 0 {
		return "off"
	}
	return strings.Join(slots, ",")
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
	if provider.IsOpenAICompatibleType(p.Type) && endpointPathIsEmpty(p.Endpoint) {
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
