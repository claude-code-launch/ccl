package cmd

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func parseModelList(modelStr string) []string {
	return modelrouting.SplitCSV(modelStr)
}

func fetchModelsForProvider(p provider.Provider) []string {
	infos := fetchModelInfosForProvider(p)
	models := make([]string, 0, len(infos))
	for _, info := range infos {
		models = append(models, info.ID)
	}
	return models
}

func fetchModelInfosForProvider(p provider.Provider) []protocol.ModelInfo {
	if provider.IsAutoClawType(p.Type) {
		// The managed proxy has a fixed account catalog; the built-in entries are
		// authoritative for the CLI and avoid probing it as a generic gateway.
		return oauthproxy.AutoClawModelCatalog()
	}
	var infos []protocol.ModelInfo
	var err error
	if provider.IsOpenAICompatibleType(p.Type) {
		infos, err = protocol.GetOpenAIModelInfos(p.Endpoint, p.APIKey)
	} else {
		infos, err = protocol.GetAnthropicModelInfosWithAuth(p.Endpoint, p.APIKey, p.AnthropicAuth)
	}
	if err != nil {
		return nil
	}
	return infos
}

// probeWireType returns the provider type a model-availability probe must use.
//
// An embedded subscription runtime serves exactly one surface — the Anthropic
// Messages endpoint Claude Code talks to — while its persisted Type is only the
// local-dispatch compatibility value (openai for Kimi/Gemini/WorkBuddy,
// openai_responses for GPT/Grok/Copilot/Zed). The loopback routes neither
// /v1/chat/completions nor /v1/responses, so probing it under the compatibility
// type 404s every model of a healthy subscription. AutoClaw keeps its own type:
// it has a dedicated probe branch that crosses the adapter with bearer auth.
func probeWireType(p provider.Provider) string {
	if strings.TrimSpace(p.OAuthProvider) == "" || provider.IsAutoClawType(p.Type) {
		return p.Type
	}
	return "anthropic"
}
