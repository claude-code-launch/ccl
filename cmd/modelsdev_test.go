package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
)

func TestModelsDevProviderToDraft(t *testing.T) {
	p := modelsdev.Provider{
		ID:   "opencode-go",
		Name: "OpenCode Go",
		NPM:  "@ai-sdk/openai-compatible",
		API:  "https://opencode.ai/zen/go/v1",
		Models: map[string]modelsdev.Model{
			"glm-5.2": {
				ID:     "glm-5.2",
				Name:   "GLM-5.2",
				Status: "deprecated",
				Limit:  modelsdev.ModelLimit{Context: 1000000, Output: 131072},
			},
			"qwen3.8-max": {
				ID:       "qwen3.8-max",
				Name:     "Qwen3.8 Max",
				Provider: &modelsdev.ModelProvider{NPM: "@ai-sdk/anthropic"},
				Limit:    modelsdev.ModelLimit{Context: 1000000, Output: 131072},
			},
			"grok-4.5": {
				ID:       "grok-4.5",
				Name:     "Grok 4.5",
				Provider: &modelsdev.ModelProvider{NPM: "@ai-sdk/openai"},
				Limit:    modelsdev.ModelLimit{Context: 500000, Output: 500000},
			},
			"kimi-k3": {
				ID:    "kimi-k3",
				Name:  "Kimi K3",
				Limit: modelsdev.ModelLimit{Context: 1048576, Output: 131072},
			},
		},
	}

	draft, metadata := modelsDevProviderToDraft(p)

	if draft.Type != "modelsdev" {
		t.Fatalf("Type = %q, want modelsdev", draft.Type)
	}
	if draft.Name != "opencode-go" || draft.Endpoint != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("draft identity wrong: %+v", draft)
	}
	// deprecated glm-5.2 must be skipped.
	if strings.Contains(draft.Model, "glm-5.2") {
		t.Fatalf("deprecated model leaked into pool: %q", draft.Model)
	}
	// Protocol table must be keyed by lowercase id.
	if got := draft.ModelProtocols["qwen3.8-max"]; got != "anthropic" {
		t.Fatalf("qwen3.8-max protocol = %q, want anthropic", got)
	}
	if got := draft.ModelProtocols["grok-4.5"]; got != "openai_responses" {
		t.Fatalf("grok-4.5 protocol = %q, want openai_responses", got)
	}
	// kimi-k3 inherits the provider default -> chat.
	if got := draft.ModelProtocols["kimi-k3"]; got != "openai" {
		t.Fatalf("kimi-k3 protocol = %q, want openai (inherited)", got)
	}
	// Pool should be kimi-k3,qwen3.8-max,grok-4.5 in sorted id order.
	if !strings.Contains(draft.Model, "grok-4.5") || !strings.Contains(draft.Model, "kimi-k3") || !strings.Contains(draft.Model, "qwen3.8-max") {
		t.Fatalf("pool missing models: %q", draft.Model)
	}
	// Metadata carries context window for the 1M advisory.
	if info, ok := metadata["qwen3.8-max"]; !ok || info.ContextWindow != 1000000 {
		t.Fatalf("qwen3.8-max metadata = %+v", metadata["qwen3.8-max"])
	}
}
