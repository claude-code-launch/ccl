package codex

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Converted models share the cached models.dev effort metadata. Provider
// metadata still supplies names, images and prices; native Responses bypasses
// this step. Unknown models retain the existing compatibility fallback.
func withModelsDevReasoning(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider) (map[string]protocol.ModelInfo, error) {
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
			providerID = configured.OAuthProvider
		}
	}
	descriptions := map[string]string{"minimal": "Minimal reasoning for fast responses"}
	for _, level := range protocol.GPTReasoningLevels() {
		descriptions[level.Effort] = level.Description
	}
	for _, id := range models {
		key := id
		if _, ok := details[key]; !ok {
			if _, ok := details[strings.ToLower(id)]; ok {
				key = strings.ToLower(id)
			}
		}
		info := details[key]
		// Gemini's account inventory defines the callable thinking variants.
		// Public metadata must not add tiers this bound account cannot route.
		if configured.OAuthProvider == "gemini" && info.ReasoningKnown {
			continue
		}
		name := names[id]
		if name == "" {
			name = info.DisplayName
		}
		efforts, found := index.Efforts(providerID, id, name)
		if !found {
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
