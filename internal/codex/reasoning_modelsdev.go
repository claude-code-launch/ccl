package codex

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Converted models share the cached models.dev effort metadata. Provider
// metadata still supplies names, images and prices when explicitly declared,
// while models.dev backfills missing modalities and output limits on known IDs.
// Native Responses bypasses effort overrides; unknown models remain text-only.
func withModelsDevReasoning(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider) (map[string]protocol.ModelInfo, error) {
	return withModelsDevMetadata(models, details, names, configured, true)
}

func withModelsDevMetadata(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider, includeReasoning bool) (map[string]protocol.ModelInfo, error) {
	index, err := modelsdev.CachedIndex()
	if err != nil {
		// Metadata is optional. A first offline setup must not block model use.
		return details, nil
	}
	if details == nil {
		details = make(map[string]protocol.ModelInfo, len(models))
	}
	providerID := configured.ModelsDevProvider
	if providerID == "" {
		if provider.IsModelsDevType(configured.Type) {
			providerID = provider.ModelsDevCatalogID(configured)
		} else {
			providerID = modelsDevOAuthProviderID(configured.OAuthProvider)
		}
	}
	descriptions := map[string]string{"minimal": "Minimal reasoning for fast responses"}
	for _, level := range protocol.GPTReasoningLevels() {
		descriptions[level.Effort] = level.Description
	}
	for _, id := range models {
		key, info, _ := lookupModelInfo(details, id)
		name := lookupModelDisplayName(names, id)
		if name == "" {
			name = info.DisplayName
		}
		modified := false
		if len(info.InputModalities) == 0 {
			if modalities, found := index.InputModalities(providerID, id, name); found && len(modalities) > 0 {
				info.InputModalities = modalities
				modified = true
			}
		}
		if info.MaxOutputTokens <= 0 {
			if maxOut, found := index.MaxOutputTokens(providerID, id, name); found && maxOut > 0 {
				info.MaxOutputTokens = maxOut
				modified = true
			}
		}
		// Gemini's account inventory defines the callable thinking variants.
		// Public metadata must not add tiers this bound account cannot route.
		if !includeReasoning || (strings.EqualFold(strings.TrimSpace(configured.OAuthProvider), "gemini") && info.ReasoningKnown) {
			if modified {
				details[key] = info
			}
			continue
		}
		efforts, found := index.Efforts(providerID, id, name)
		if !found {
			if modified {
				details[key] = info
			}
			continue
		}
		info.ReasoningKnown = true
		info.ReasoningLevels = nil
		chosen := strings.ToLower(strings.TrimSpace(info.DefaultReasoningLevel))
		validDefault := false
		for _, effort := range efforts {
			info.ReasoningLevels = append(info.ReasoningLevels, protocol.ReasoningLevel{Effort: effort, Description: descriptions[effort]})
			validDefault = validDefault || effort == chosen
		}
		if !validDefault {
			chosen = ""
			if len(efforts) > 0 {
				chosen = efforts[0]
				for _, effort := range efforts {
					if effort == protocol.GPTDefaultReasoningLevel {
						chosen = effort
						break
					}
				}
			}
		}
		info.DefaultReasoningLevel = chosen
		details[key] = info
	}
	return details, nil
}

func modelsDevOAuthProviderID(oauth string) string {
	switch strings.ToLower(strings.TrimSpace(oauth)) {
	case "grok", "xai":
		return "xai"
	case "gpt", "codex":
		return "openai"
	case "gemini":
		return "google"
	default:
		return oauth
	}
}
