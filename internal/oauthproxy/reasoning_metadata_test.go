package oauthproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkBuddyReasoningCapabilitiesSurviveModelDiscovery(t *testing.T) {
	_, _, _, details := buildWorkBuddyRoutes("", []workbuddyModel{{ID: "reasoner", Name: "Reasoner", SupportsReasoning: true}, {ID: "plain", Name: "Plain"}})
	info := details["reasoner"]
	if len(info.ReasoningLevels) != 3 || info.DefaultReasoningLevel != "medium" {
		t.Fatal("supportsReasoning did not reach model details")
	}
	if len(details["plain"].ReasoningLevels) != 0 {
		t.Fatal("plain model acquired unsupported reasoning controls")
	}
	runtime := &Runtime{modelDetails: details}
	copy := runtime.ModelDetails()
	info = copy["reasoner"]
	info.ReasoningLevels[0].Effort = "modified"
	if runtime.ModelDetails()["reasoner"].ReasoningLevels[0].Effort != "low" {
		t.Fatal("caller modified runtime reasoning capabilities")
	}
}

func TestCodexAccountDiscoveryPreservesReasoningMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"models": []any{map[string]any{"slug": "account-reasoner", "visibility": "list", "context_window": 200000, "default_reasoning_level": "high", "supported_reasoning_levels": []any{map[string]any{"effort": "medium", "description": "Balanced"}, map[string]any{"effort": "high", "description": "Deep"}, map[string]any{"effort": "xhigh", "description": "Extra deep"}}}}})
	}))
	defer server.Close()
	infos, status, err := requestCodexModelInfos(context.Background(), server.Client(), server.URL+"/v1", codexResponsesAuthorization{token: "fake-key"})
	if err != nil || status != 200 || len(infos) != 1 {
		t.Fatalf("catalog: %d %v", status, err)
	}
	if infos[0].ContextWindow != 200000 || infos[0].DefaultReasoningLevel != "high" || len(infos[0].ReasoningLevels) != 3 {
		t.Fatal("native account metadata was discarded")
	}
}

func TestCodexBridgeAdaptiveEffortReachesChatUpstream(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		raw, _ := json.Marshal(map[string]any{"model": "reasoner", "max_tokens": 4096, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}, "thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": effort}})
		converted, err := convertAnthropicToChatCompletions(raw)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if json.Unmarshal(converted.body, &body) != nil {
			t.Fatal("invalid Chat JSON")
		}
		if body["reasoning_effort"] != effort {
			t.Fatalf("effort = %v, want %q", body["reasoning_effort"], effort)
		}
	}
}
