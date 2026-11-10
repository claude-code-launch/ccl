package codex

import (
	"net/http"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestCodexGatewayPublishesContextAndClientSearchCapabilities(t *testing.T) {
	for _, protocol := range []string{"openai", "openai_responses"} {
		t.Run(protocol, func(t *testing.T) {
			g := fixtureProviderGateway(t, func(http.ResponseWriter, *http.Request) {}, provider.Provider{Name: "chosen", Type: protocol, Model: "model-a", ContextPreset: provider.ContextPresetNameBalanced800K}, Options{})
			model := g.Models()[0]
			if model["context_window"] != 800_000 || model["auto_compact_token_limit"] != 680_000 || model["supports_search_tool"] != (protocol != "openai_responses") {
				t.Fatalf("provider=%s model=%+v", protocol, model)
			}
		})
	}
}

func TestCodexCatalogUsesProviderContextAndCompactionPolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		id       string
		provider provider.Provider
		window   int
	}{
		{"default", "model-a", provider.Provider{}, 200_000},
		{"marked catalog ID", "model-a[1m]", provider.Provider{}, 1_000_000},
		{"marked pool", "model-a", provider.Provider{Model: "other,Model-A[1m]"}, 1_000_000},
		{"marked slot", "model-a", provider.Provider{SonnetModel: "model-a[1m]"}, 1_000_000},
		{"other slot", "model-a", provider.Provider{OpusModel: "other[1m]"}, 200_000},
		{"balanced 500K", "model-a", provider.Provider{ContextPreset: provider.ContextPresetNameBalanced500K}, 500_000},
		{"balanced caps marked slot", "model-a", provider.Provider{ContextPreset: provider.ContextPresetNameBalanced800K, SonnetModel: "model-a[1m]"}, 800_000},
		{"legacy balanced env", "model-a", provider.Provider{Env: provider.ContextPresetEnv(provider.Provider{ContextPreset: provider.ContextPresetNameBalanced500K})}, 500_000},
		{"retired env follows default", "model-a", provider.Provider{Env: map[string]string{provider.EnvMaxContextTokens: "invalid"}}, 200_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Metadata from a backend is a capability, not the user's budget.
			for _, advertised := range []int{0, 32_768, 1_000_000} {
				model := newModel("ccl/chosen/"+test.id, test.id, map[string]protocol.ModelInfo{test.id: {ContextWindow: advertised}}, nil)
				applyProviderContext(model, test.id, test.provider)
				if model["context_window"] != test.window || model["max_context_window"] != test.window || model["auto_compact_token_limit"] != test.window*85/100 {
					t.Fatalf("advertised=%d model=%+v", advertised, model)
				}
			}
		})
	}
}
