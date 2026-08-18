package cmd

import (
	"context"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// modelsDevProviders fetches the models.dev catalog and returns it as a stable,
// name-sorted slice. The entry point is a single unfiltered list so the user
// can pick any provider models.dev knows about.
func modelsDevProviders(ctx context.Context) ([]modelsdev.Provider, error) {
	catalog, err := modelsdev.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	providers := make([]modelsdev.Provider, 0, len(catalog))
	for _, p := range catalog {
		providers = append(providers, p)
	}
	sort.Slice(providers, func(i, j int) bool {
		a, b := providers[i].Name, providers[j].Name
		if strings.EqualFold(a, b) {
			return providers[i].ID < providers[j].ID
		}
		return strings.ToLower(a) < strings.ToLower(b)
	})
	return providers, nil
}

// modelsDevProviderToDraft converts a models.dev provider into a ccl provider
// draft plus its per-model display metadata. Only models whose AI SDK package
// maps to a known ccl protocol are included; deprecated models are skipped. The
// resulting provider uses the mixed-protocol "modelsdev" type and carries the
// per-model protocol table the runtime needs to route each request.
func modelsDevProviderToDraft(p modelsdev.Provider) (provider.Provider, map[string]protocol.ModelInfo) {
	draft := provider.Provider{
		Name:           p.ID,
		Type:           "modelsdev",
		Endpoint:       p.API,
		ModelProtocols: make(map[string]string),
	}
	metadata := make(map[string]protocol.ModelInfo)
	var pool []string

	ids := make([]string, 0, len(p.Models))
	for id := range p.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		model := p.Models[id]
		if strings.EqualFold(strings.TrimSpace(model.Status), "deprecated") {
			continue
		}
		npm := modelsdev.ResolvedNPM(p, model)
		proto, ok := provider.ProtocolForAISdkNPM(npm)
		if !ok {
			continue
		}
		modelID := model.ID
		if modelID == "" {
			modelID = id
		}
		draft.ModelProtocols[strings.ToLower(modelID)] = proto
		pool = append(pool, modelID)
		metadata[strings.ToLower(modelID)] = protocol.ModelInfo{
			ID:              modelID,
			DisplayName:     model.Name,
			ContextWindow:   model.Limit.Context,
			MaxOutputTokens: model.Limit.Output,
		}
	}
	draft.Model = strings.Join(pool, ",")
	return draft, metadata
}
