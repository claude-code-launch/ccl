package oauthproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func useAutoClawRuntimeConfig(t *testing.T, path string) {
	t.Helper()
	original := autoClawRuntimeConfigPath
	autoClawRuntimeConfigPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { autoClawRuntimeConfigPath = original })
}

func TestAutoClawModelCatalogMatchesManagedProvider(t *testing.T) {
	useAutoClawRuntimeConfig(t, filepath.Join(t.TempDir(), "missing.json"))
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
	useAutoClawRuntimeConfig(t, filepath.Join(t.TempDir(), "missing.json"))
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
	useAutoClawRuntimeConfig(t, filepath.Join(t.TempDir(), "missing.json"))
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
	useAutoClawRuntimeConfig(t, filepath.Join(t.TempDir(), "missing.json"))
	raw := []byte(`{"model":"zai_auto","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]}]}`)
	normalized, err := normalizeAutoClawBody(raw)
	if err != nil {
		t.Fatalf("normalizeAutoClawBody() error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(normalized, &body); err != nil {
		t.Fatalf("normalized body is invalid JSON: %v", err)
	}
	if body["model"] != "auto" {
		t.Fatalf("normalized model = %v", body["model"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("managed ZAI body unexpectedly contains stream_options: %s", normalized)
	}
	messages := body["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if reasoning, exists := assistant["reasoning_content"]; !exists || reasoning != "" {
		t.Fatalf("assistant reasoning_content = %#v, want an explicit empty string", reasoning)
	}
}

func TestNormalizeAutoClawRequestRoutesImagesToConfiguredImageModel(t *testing.T) {
	useAutoClawRuntimeConfig(t, filepath.Join(t.TempDir(), "missing.json"))
	contract, _ := autoClawEffectiveContract()
	converted := &chatCompletionsConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{upstreamModel: "zai_auto"},
		model:                   "zai_auto",
		body:                    []byte(`{"model":"zai_auto","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`),
	}
	if err := normalizeAutoClawRequest(converted, contract); err != nil {
		t.Fatal(err)
	}
	if converted.model != "zai_auto-fast" || converted.upstreamModel != "zai_auto-fast" {
		t.Fatalf("image route = model %q upstream %q", converted.model, converted.upstreamModel)
	}
	var body map[string]any
	if err := json.Unmarshal(converted.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "auto-fast" {
		t.Fatalf("image body model = %v", body["model"])
	}
}

func TestAutoClawLoadsGeneratedOpenClawContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openclaw.runtime.json")
	config := `{
		"models":{"providers":{"zai":{"baseUrl":"https://future.autoglm.ai/autoclaw-proxy/proxy/autoclaw","api":"openai-completions","models":[
			{"id":"zai_future","name":"Future","input":["text"],"reasoning":true,"contextWindow":2000000,"maxTokens":200000,"headers":{"X-Version":"2.0.0"},"compat":{"requiresReasoningContentOnAssistantMessages":true}},
			{"id":"zai_future-vision","name":"Future Vision","input":["text","image"],"reasoning":true,"contextWindow":2000000,"maxTokens":200000,"headers":{"X-Version":"2.0.0"}}
		]}}},
		"agents":{"defaults":{"imageModel":{"primary":"zai/zai_future-vision"}}}
	}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	useAutoClawRuntimeConfig(t, path)

	contract, local := autoClawEffectiveContract()
	if !local || contract.version != "2.0.0" || contract.imageModel != "zai_future-vision" || len(contract.models) != 2 {
		t.Fatalf("local contract = %+v local=%t", contract, local)
	}
	if AutoClawOpenAIBaseURL() != "https://future.autoglm.ai/autoclaw-proxy/proxy/autoclaw" || autoClawInstalledVersion() != "2.0.0" {
		t.Fatalf("dynamic endpoint/version = %q / %q", AutoClawOpenAIBaseURL(), autoClawInstalledVersion())
	}
	if !AutoClawSupportsModel("zai_future") || AutoClawSupportsModel("zai_auto") {
		t.Fatalf("dynamic catalog IDs = %v", AutoClawModelIDs())
	}
	if !autoClawModelSupportsImage(contract.models, autoClawPreferredImageModel(contract)) {
		t.Fatal("generated image model was not adopted")
	}
}
