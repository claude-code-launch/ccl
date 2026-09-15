package oauthproxy

import (
	"encoding/json"
	"testing"
)

func TestAutoClawModelCatalogMatchesManagedProvider(t *testing.T) {
	want := []struct {
		id        string
		name      string
		context   int
		maxOutput int
	}{
		{"zai_auto", "Auto", 1_048_576, 131_072},
		{"zai_auto-fast", "Auto-Fast", 1_048_576, 393_216},
		{"zaicoding_glm-5.3", "GLM-5.3", 1_048_576, 307_200},
		{"tdpsk_deepseek-v4-flash-202605", "Deepseek-V4.1-Flash", 1_048_576, 393_216},
		{"tdpsk_deepseek-v4-pro-202606", "DeepSeek-V4-Pro", 1_048_576, 393_216},
		{"zai_glm-5.3-flash", "GLM-5.3-Flash", 1_048_576, 131_072},
	}
	catalog := AutoClawModelCatalog()
	if len(catalog) != len(want) {
		t.Fatalf("catalog size = %d, want %d", len(catalog), len(want))
	}
	for i, expected := range want {
		got := catalog[i]
		if got.ID != expected.id || got.DisplayName != expected.name || got.ContextWindow != expected.context || got.MaxOutputTokens != expected.maxOutput {
			t.Fatalf("catalog[%d] = %#v, want %#v", i, got, expected)
		}
	}
}

func TestAutoClawSupportsManagedModelIDsCaseInsensitive(t *testing.T) {
	for _, model := range []string{"zai_auto", "ZAI_AUTO-FAST", "zaicoding_GLM-5.3", "tdpsk_deepseek-v4-pro-202606"} {
		if !AutoClawSupportsModel(model) {
			t.Fatalf("AutoClawSupportsModel(%q) = false", model)
		}
	}
	for _, model := range []string{"", "gpt-5", "zai_glm-5.4"} {
		if AutoClawSupportsModel(model) {
			t.Fatalf("AutoClawSupportsModel(%q) = true", model)
		}
	}
	for _, model := range []string{"GLM-5.3", "glm-5.3-flash", "GLM-5-Turbo"} {
		if !AutoClawSupportsModel(model) {
			t.Fatalf("legacy AutoClaw model alias %q was not accepted", model)
		}
	}
}

func TestAutoClawBodyModelRemovesProviderPrefix(t *testing.T) {
	cases := map[string]string{
		"zai_auto":                       "auto",
		"zai_auto-fast":                  "auto-fast",
		"zaicoding_glm-5.3":              "glm-5.3",
		"tdpsk_deepseek-v4-flash-202605": "deepseek-v4-flash-202605",
		"zai_glm-5.3-flash":              "glm-5.3-flash",
		"custom-model":                   "custom-model",
		"uppercase_prefix_model":         "prefix_model",
	}
	for route, want := range cases {
		if got := autoClawBodyModel(route); got != want {
			t.Errorf("autoClawBodyModel(%q) = %q, want %q", route, got, want)
		}
	}
}

func TestNormalizeAutoClawBodyMatchesManagedZAIShape(t *testing.T) {
	raw := []byte(`{"model":"zai_glm-5.3-flash","stream":true,"stream_options":{"include_usage":true}}`)
	normalized, err := normalizeAutoClawBody(raw)
	if err != nil {
		t.Fatalf("normalizeAutoClawBody() error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(normalized, &body); err != nil {
		t.Fatalf("normalized body is invalid JSON: %v", err)
	}
	if body["model"] != "glm-5.3-flash" {
		t.Fatalf("normalized model = %v", body["model"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("managed ZAI body unexpectedly contains stream_options: %s", normalized)
	}
}
