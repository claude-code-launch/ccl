package cmd

import (
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
	if provider.IsCommandCodeType(p.Type) {
		// The Command Code gateway has no upstream model list; the runtime's
		// static catalog is the authoritative one for the whole CLI as well.
		return oauthproxy.CommandCodeModelCatalog()
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
