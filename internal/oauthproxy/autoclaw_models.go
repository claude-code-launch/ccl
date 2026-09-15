package oauthproxy

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

// autoclawModelDefinition mirrors one managed model from AutoClaw's generated
// zai provider catalog. ID is the routing value sent in X-Request-Model; the
// JSON body receives the same value with its provider prefix removed.
type autoclawModelDefinition struct {
	id        string
	name      string
	context   int
	maxOutput int
}

var autoclawModelCatalog = []autoclawModelDefinition{
	{id: "zai_auto", name: "Auto", context: 1_048_576, maxOutput: 131_072},
	{id: "zai_auto-fast", name: "Auto-Fast", context: 1_048_576, maxOutput: 393_216},
	{id: "zaicoding_glm-5.3", name: "GLM-5.3", context: 1_048_576, maxOutput: 307_200},
	{id: "tdpsk_deepseek-v4-flash-202605", name: "Deepseek-V4.1-Flash", context: 1_048_576, maxOutput: 393_216},
	{id: "tdpsk_deepseek-v4-pro-202606", name: "DeepSeek-V4-Pro", context: 1_048_576, maxOutput: 393_216},
	{id: "zai_glm-5.3-flash", name: "GLM-5.3-Flash", context: 1_048_576, maxOutput: 131_072},
}

// AutoClawModelCatalog returns the built-in model catalog as ModelInfo entries,
// including the context window and output limit ccl reports for each model.
func AutoClawModelCatalog() []protocol.ModelInfo {
	infos := make([]protocol.ModelInfo, 0, len(autoclawModelCatalog))
	for _, model := range autoclawModelCatalog {
		infos = append(infos, protocol.ModelInfo{
			ID:              model.id,
			DisplayName:     model.name,
			ContextWindow:   model.context,
			MaxOutputTokens: model.maxOutput,
		})
	}
	return infos
}

// AutoClawModelIDs returns the catalog model IDs in catalog order.
func AutoClawModelIDs() []string {
	ids := make([]string, 0, len(autoclawModelCatalog))
	for _, model := range autoclawModelCatalog {
		ids = append(ids, model.id)
	}
	return ids
}

// AutoClawSupportsModel reports whether the catalog admits a model ID,
// case-insensitively. Model availability uses catalog membership rather than a
// live probe: these route IDs come from the AutoClaw-managed provider catalog
// and the public proxy has no model-list endpoint for this account surface.
func AutoClawSupportsModel(model string) bool {
	model = autoClawCanonicalModel(model)
	for _, entry := range autoclawModelCatalog {
		if strings.EqualFold(entry.id, model) {
			return true
		}
	}
	return false
}
