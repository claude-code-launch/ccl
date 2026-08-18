package modelsdev

import (
	"encoding/json"
	"testing"
)

func TestResolvedNPM(t *testing.T) {
	p := Provider{NPM: "@ai-sdk/openai-compatible"}

	t.Run("inherits provider default", func(t *testing.T) {
		m := Model{ID: "glm-5.2"}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/openai-compatible" {
			t.Fatalf("ResolvedNPM = %q, want provider default", got)
		}
	})

	t.Run("model override wins", func(t *testing.T) {
		m := Model{ID: "qwen3.8-max", Provider: &ModelProvider{NPM: "@ai-sdk/anthropic"}}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/anthropic" {
			t.Fatalf("ResolvedNPM = %q, want model override", got)
		}
	})

	t.Run("empty model override falls back", func(t *testing.T) {
		m := Model{ID: "grok-4.5", Provider: &ModelProvider{NPM: "  "}}
		if got := ResolvedNPM(p, m); got != "@ai-sdk/openai-compatible" {
			t.Fatalf("ResolvedNPM = %q, want provider default for blank override", got)
		}
	})
}

// TestFetchDecode exercises the raw-map decoding path with a fixture that mixes
// a valid provider with a malformed top-level entry that must be skipped.
func TestFetchDecode(t *testing.T) {
	valid := `{"id":"opencode-go","name":"OpenCode Go","npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/zen/go/v1","env":["OPENCODE_API_KEY"],"models":{"glm-5.2":{"id":"glm-5.2","name":"GLM-5.2","status":"deprecated","limit":{"context":1000000,"output":131072}}}}`
	catalog := map[string]json.RawMessage{
		"opencode-go": json.RawMessage(valid),
		"junk":        json.RawMessage(`"not a provider"`),
		"no-models":   json.RawMessage(`{"id":"x","api":"https://example.com"}`),
	}

	providers := make(map[string]Provider, len(catalog))
	for id, entry := range catalog {
		var p Provider
		if err := json.Unmarshal(entry, &p); err != nil {
			continue
		}
		if p.API == "" || len(p.Models) == 0 {
			continue
		}
		if p.ID == "" {
			p.ID = id
		}
		providers[id] = p
	}

	if len(providers) != 1 {
		t.Fatalf("decoded %d providers, want 1", len(providers))
	}
	p, ok := providers["opencode-go"]
	if !ok {
		t.Fatal("missing opencode-go provider")
	}
	if p.Name != "OpenCode Go" || p.API != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("provider decoded wrong: %+v", p)
	}
	m := p.Models["glm-5.2"]
	if m.Limit.Context != 1000000 || m.Limit.Output != 131072 {
		t.Fatalf("model limit decoded wrong: %+v", m.Limit)
	}
}
