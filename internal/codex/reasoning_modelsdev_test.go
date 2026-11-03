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
