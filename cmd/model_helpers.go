package cmd

import (
	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func parseModelList(modelStr string) []string {
	return modelrouting.SplitCSV(modelStr)
}

func fetchModelsForProvider(p provider.Provider) []string {
	var modelsStr string
	var err error
	if provider.IsOpenAICompatibleType(p.Type) {
		modelsStr, err = protocol.GetOpenAIModels(p.Endpoint, p.APIKey)
	} else {
		modelsStr, err = protocol.GetAnthropicModelsWithAuth(p.Endpoint, p.APIKey, p.AnthropicAuth)
	}
	if err != nil || modelsStr == "" {
		return nil
	}
	return parseModelList(modelsStr)
}
