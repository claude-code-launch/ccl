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
	infos, err := protocol.GetOpenAIModelInfos(p.Endpoint, p.APIKey)
	if err != nil {
		return nil
	}
	return infos
}

// probeWireType uses the common Responses endpoint of OAuth runtimes.
func probeWireType(p provider.Provider) string {
	if strings.TrimSpace(p.OAuthProvider) == "" {
		return p.Type
	}
	return "openai_responses"
}
