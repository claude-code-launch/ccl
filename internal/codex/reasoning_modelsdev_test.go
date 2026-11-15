package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestModelsDevEffortsApplyOnlyToConvertedRoutes(t *testing.T) {
	seedModelsDevCache(t)
	const id = "deepseek-v4.1-flash"
	for _, native := range []bool{false, true} {
		name := "converted"
		if native {
			name = "native-responses"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.NotFoundHandler())
			defer server.Close()
			start, kind := oauthproxy.StartOpenAIChatAPI, "openai"
			if native {
				start, kind = oauthproxy.StartOpenAIResponsesAPI, "openai_responses"
			}
			runtime, err := start(context.Background(), server.URL, "fake-key", id)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Stop()
			gateway, err := New(context.Background(), runtime, provider.Provider{Name: "fixture", Type: kind, Model: id}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			levels := gateway.Models()[0]["supported_reasoning_levels"].([]protocol.ReasoningLevel)
			if native {
				if !slices.Equal(levels, protocol.GPTReasoningLevels()) {
					t.Fatal("models.dev metadata changed the native Responses path")
				}
			} else if len(levels) != 3 || levels[1].Effort != "high" || levels[2].Effort != "max" {
				t.Fatalf("gateway did not use models.dev efforts: %v", levels)
			}
		})
	}
}

func TestModelsDevReasoningOverridesGenericProviderLadder(t *testing.T) {
	seedModelsDevCache(t)
	models := []string{"deepseek-v4.1-flash", "space-bunny", "renamed-revision", "unknown-model"}
	details := map[string]protocol.ModelInfo{}
	for _, id := range models {
		details[id] = protocol.ModelInfo{ID: id, ContextWindow: 12345, ReasoningLevels: protocol.ChatReasoningLevels(), DefaultReasoningLevel: "medium"}
	}
	names := map[string]string{"renamed-revision": "GLM-5.3-Flash"}
	details, err := withModelsDevReasoning(models, details, names, provider.Provider{OAuthProvider: "workbuddycn"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id      string
		efforts []string
		chosen  string
		known   bool
	}{
		{"deepseek-v4.1-flash", []string{"low", "high", "max"}, "low", true},
		{"space-bunny", []string{"low", "medium", "high", "xhigh", "max"}, "medium", true},
		{"renamed-revision", []string{"low", "high", "max"}, "low", true},
		{"unknown-model", []string{"low", "medium", "high"}, "medium", false},
	} {
		info := details[test.id]
		model := newModel(test.id, test.id, details, names)
		var efforts []string
		for _, level := range model["supported_reasoning_levels"].([]protocol.ReasoningLevel) {
			efforts = append(efforts, level.Effort)
			if level.Description == "" {
				t.Errorf("%s: missing description", test.id)
			}
		}
		if !slices.Equal(efforts, test.efforts) || model["default_reasoning_level"] != test.chosen || info.ReasoningKnown != test.known || info.ContextWindow != 12345 {
			t.Errorf("%s: efforts=%v default=%v metadata=%+v", test.id, efforts, model["default_reasoning_level"], info)
		}
	}
}

func TestModelsDevKnownEmptyEffortsDoNotAcquireGPTDefaults(t *testing.T) {
	seedModelsDevCache(t)
	const id = "deepseek-ai/DeepSeek-V4.1-Flash"
	details, err := withModelsDevReasoning([]string{id}, nil, nil, provider.Provider{ModelsDevProvider: "wandb"})
	if err != nil {
		t.Fatal(err)
	}
	// The canonical owner/reference fallback may offer effort tiers, but an
	// exact provider's toggle-only metadata must remain authoritative.
	info := details[id]
	if !info.ReasoningKnown || len(info.ReasoningLevels) != 0 {
		t.Fatalf("provider's empty effort metadata was replaced: %+v", info)
	}
	details = withGPTReasoningDefaults([]string{id}, details)
	if len(details[id].ReasoningLevels) != 0 {
		t.Fatal("GPT compatibility fallback invented effort controls")
	}
}

func seedModelsDevCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../modelsdev/modelsdev.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiAccountEffortsOverridePublicMetadata(t *testing.T) {
	seedModelsDevCache(t)
	// Use an ID present in the public fixture to prove the override boundary,
	// independently of which Gemini generations models.dev happens to publish.
	const id = "deepseek-v4.1-flash"
	details := map[string]protocol.ModelInfo{id: {ID: id, ReasoningKnown: true, ReasoningLevels: []protocol.ReasoningLevel{{Effort: "low"}, {Effort: "high"}}, DefaultReasoningLevel: "high"}}
	actual, err := withModelsDevReasoning([]string{id}, details, nil, provider.Provider{OAuthProvider: "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	if info := actual[id]; len(info.ReasoningLevels) != 2 || info.ReasoningLevels[1].Effort != "high" || info.DefaultReasoningLevel != "high" {
		t.Fatalf("public metadata replaced account variants: %+v", info)
	}
}

func TestModelsDevBackfillsModalitiesAndOutputLimitsWithoutInventingUnknownVision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := `{"xai":{"models":{"grok-vision":{"modalities":{"input":["text","image"],"output":["text"]},"limit":{"context":256000,"output":65536},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}]}}},"openai":{"models":{"vision-responses":{"modalities":{"input":["text","image"],"output":["text"]},"limit":{"output":49152},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low"]}]}}}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	details, err := withModelsDevMetadata([]string{"grok-vision", "unknown-local-model"}, nil, nil, provider.Provider{OAuthProvider: "grok"}, true)
	if err != nil {
		t.Fatal(err)
	}
	visionModel := newModel("ccl/grok/grok-vision", "grok-vision", details, nil)
	if !slices.Equal(visionModel["input_modalities"].([]string), []string{"text", "image"}) || visionModel["supports_image_detail_original"] != true || details["grok-vision"].MaxOutputTokens != 65536 {
		t.Fatalf("expected vision + max output backfill on grok-vision: model=%+v info=%+v", visionModel, details["grok-vision"])
	}
	unknownModel := newModel("ccl/grok/unknown-local-model", "unknown-local-model", details, nil)
	if !slices.Equal(unknownModel["input_modalities"].([]string), []string{"text"}) || unknownModel["supports_image_detail_original"] != false {
		t.Fatalf("unknown model must stay text-only: %+v", unknownModel)
	}

	// Native Responses routes receive missing vision/output metadata while
	// preserving their native GPT effort ladder.
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	runtime, err := oauthproxy.StartOpenAIResponsesAPI(context.Background(), server.URL, "fake-key", "vision-responses")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	gateway, err := New(context.Background(), runtime, provider.Provider{Name: "resp", Type: "openai_responses", Model: "vision-responses", ModelsDevProvider: "openai"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	nativeEntry := gateway.Models()[0]
	if !slices.Equal(nativeEntry["input_modalities"].([]string), []string{"text", "image"}) || nativeEntry["supports_image_detail_original"] != true {
		t.Fatalf("native responses route missed vision backfill: %+v", nativeEntry)
	}
	if !slices.Equal(nativeEntry["supported_reasoning_levels"].([]protocol.ReasoningLevel), protocol.GPTReasoningLevels()) {
		t.Fatalf("native responses effort ladder was modified: %+v", nativeEntry["supported_reasoning_levels"])
	}
}

func TestMultiModelPoolPreservesGPTDefaultsForUnrecognizedSiblingsAndOneMVariants(t *testing.T) {
	seedModelsDevCache(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	// Pool mixes: (1) models.dev-matched model with [1m] suffix, (2) models.dev toggle-only model, (3) brand-new unrecognized model.
	const spec = "space-bunny[1m],deepseek-ai/DeepSeek-V4.1-Flash,future-unlisted-model"
	runtime, err := oauthproxy.StartOpenAIChatAPI(context.Background(), server.URL, "fake-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	gateway, err := New(context.Background(), runtime, provider.Provider{Name: "pool", Type: "openai", Model: spec, ModelsDevProvider: "wandb"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	bySlug := map[string]Model{}
	for _, entry := range gateway.Models() {
		bySlug[entry["slug"].(string)] = entry
	}
	// 1. Recognized [1m] variant resolves canonical models.dev efforts and 1M context.
	bunny1M := bySlug["ccl/pool/space-bunny[1m]"]
	if bunny1M["context_window"] != 1_000_000 || len(bunny1M["supported_reasoning_levels"].([]protocol.ReasoningLevel)) != 5 {
		t.Fatalf("1M variant lost context or models.dev efforts: %+v", bunny1M)
	}
	// 2. Authoritative empty effort list from wandb provider stays empty (not filled with GPT defaults).
	toggleOnly := bySlug["ccl/pool/deepseek-ai/DeepSeek-V4.1-Flash"]
	if len(toggleOnly["supported_reasoning_levels"].([]protocol.ReasoningLevel)) != 0 || toggleOnly["default_reasoning_level"] != nil {
		t.Fatalf("known toggle-only model gained effort levels: %+v", toggleOnly)
	}
	// 3. Unrecognized sibling model in a runtime with no catalog levels keeps GPT compatibility defaults.
	unlisted := bySlug["ccl/pool/future-unlisted-model"]
	if !slices.Equal(unlisted["supported_reasoning_levels"].([]protocol.ReasoningLevel), protocol.GPTReasoningLevels()) || unlisted["default_reasoning_level"] != protocol.GPTDefaultReasoningLevel {
		t.Fatalf("unrecognized sibling lost GPT compatibility effort controls: %+v", unlisted)
	}

	// Direct [1m] lookup against bare-ID runtime ModelDetails inherits runtime metadata.
	bareDetails := map[string]protocol.ModelInfo{
		"claude-sonnet-4-6": {
			ID:                    "claude-sonnet-4-6",
			DisplayName:           "Claude Sonnet 4.6",
			ContextWindow:         200_000,
			MaxOutputTokens:       64_000,
			InputModalities:       []string{"text", "image"},
			ReasoningLevels:       protocol.ChatReasoningLevels(),
			DefaultReasoningLevel: "high",
		},
	}
	marked := newModel("ccl/work/claude-sonnet-4-6[1m]", "claude-sonnet-4-6[1m]", bareDetails, map[string]string{"claude-sonnet-4-6": "Claude Sonnet 4.6"})
	if marked["display_name"] != "Claude Sonnet 4.6" || marked["supports_image_detail_original"] != true || marked["default_reasoning_level"] != "high" {
		t.Fatalf("[1m] model did not inherit bare runtime ModelDetails: %+v", marked)
	}
}
