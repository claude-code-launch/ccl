package protocol_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestGetOpenAIModelsAppendsModelsToRootEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()

	models, err := protocol.GetOpenAIModels(server.URL, "test-key")
	if err != nil {
		t.Fatalf("GetOpenAIModels failed: %v", err)
	}
	if models != "gpt-4o" {
		t.Fatalf("unexpected models: %s", models)
	}
}

func TestGetOpenAIModelsPreservesVersionedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"v3-model"}]}`))
	}))
	defer server.Close()

	models, err := protocol.GetOpenAIModels(server.URL+"/v3", "test-key")
	if err != nil {
		t.Fatalf("GetOpenAIModels failed: %v", err)
	}
	if models != "v3-model" {
		t.Fatalf("unexpected models: %s", models)
	}
}

func TestGetAnthropicModelsNormalizesV1Endpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("missing x-api-key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4"}]}`))
	}))
	defer server.Close()

	models, err := protocol.GetAnthropicModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("GetAnthropicModels failed: %v", err)
	}
	if models != "claude-sonnet-4" {
		t.Fatalf("unexpected models: %s", models)
	}
}

func TestNormalizeVersionedURLs(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "openai models from v3 base",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/models",
		},
		{
			name: "openai chat from v3 base",
			got:  protocol.NormalizeOpenAIChatCompletionsURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/chat/completions",
		},
		{
			name: "openai responses from v3 base",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/responses",
		},
		{
			name: "openai models from v3 chat",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com/api/v3/chat/completions"),
			want: "https://example.com/api/v3/models",
		},
		{
			name: "openai responses from v3 chat",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com/api/v3/chat/completions"),
			want: "https://example.com/api/v3/responses",
		},
		{
			name: "anthropic messages from v3 base",
			got:  protocol.NormalizeAnthropicMessagesURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/messages",
		},
		{
			name: "anthropic models from v3 messages",
			got:  protocol.NormalizeAnthropicModelsURL("https://example.com/api/v3/messages"),
			want: "https://example.com/api/v3/models",
		},
		{
			name: "unversioned openai models appends directly",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com"),
			want: "https://example.com/models",
		},
		{
			name: "unversioned openai chat appends directly",
			got:  protocol.NormalizeOpenAIChatCompletionsURL("https://example.com"),
			want: "https://example.com/chat/completions",
		},
		{
			name: "unversioned openai responses appends directly",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com"),
			want: "https://example.com/responses",
		},
		{
			name: "unversioned anthropic messages appends v1",
			got:  protocol.NormalizeAnthropicMessagesURL("https://example.com"),
			want: "https://example.com/v1/messages",
		},
		{
			name: "unversioned anthropic models appends v1",
			got:  protocol.NormalizeAnthropicModelsURL("https://example.com"),
			want: "https://example.com/v1/models",
		},
		{
			name: "anthropic suffix messages appends v1",
			got:  protocol.NormalizeAnthropicMessagesURL("https://example.com/api/anthropic"),
			want: "https://example.com/api/anthropic/v1/messages",
		},
		{
			name: "anthropic suffix models appends v1",
			got:  protocol.NormalizeAnthropicModelsURL("https://example.com/api/anthropic"),
			want: "https://example.com/api/anthropic/v1/models",
		},
		{
			name: "claude suffix models appends v1",
			got:  protocol.NormalizeAnthropicModelsURL("https://example.com/api/claude"),
			want: "https://example.com/api/claude/v1/models",
		},
		{
			name: "claude suffix messages appends v1",
			got:  protocol.NormalizeAnthropicMessagesURL("https://example.com/api/claude"),
			want: "https://example.com/api/claude/v1/messages",
		},
		{
			name: "empty openai uses official v1 default",
			got:  protocol.NormalizeOpenAIModelsURL(""),
			want: "https://api.openai.com/v1/models",
		},
		{
			name: "anthropic claude base strips v1",
			got:  protocol.NormalizeAnthropicBaseURLForClaude("https://token.sensenova.cn/v1"),
			want: "https://token.sensenova.cn",
		},
		{
			name: "anthropic claude base strips v1 messages",
			got:  protocol.NormalizeAnthropicBaseURLForClaude("https://token.sensenova.cn/v1/messages"),
			want: "https://token.sensenova.cn",
		},
		{
			name: "anthropic claude base strips v1 models",
			got:  protocol.NormalizeAnthropicBaseURLForClaude("https://token.sensenova.cn/v1/models"),
			want: "https://token.sensenova.cn",
		},
		{
			name: "anthropic claude base preserves custom path",
			got:  protocol.NormalizeAnthropicBaseURLForClaude("https://example.com/api/v3"),
			want: "https://example.com/api/v3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestDedicatedCodexEndpointClassification(t *testing.T) {
	if !protocol.IsCodexBaseEndpoint("https://new.sharedchat.cc/codex") {
		t.Fatal("expected /codex to be classified as a dedicated Codex base endpoint")
	}
	if protocol.IsCodexBaseEndpoint("https://new.sharedchat.cc/codex/v1") {
		t.Fatal("/codex/v1 must not be classified as a valid Codex base endpoint")
	}

	for _, endpoint := range []string{
		"https://new.sharedchat.cc/codex/v1",
		"https://new.sharedchat.cc/codex/v1/models",
		"https://new.sharedchat.cc/codex/v1/responses",
	} {
		suggestion, invalid := protocol.InvalidCodexV1EndpointSuggestion(endpoint)
		if !invalid || suggestion != "https://new.sharedchat.cc/codex" {
			t.Errorf("InvalidCodexV1EndpointSuggestion(%q) = %q, %t", endpoint, suggestion, invalid)
		}
	}
	if _, invalid := protocol.InvalidCodexV1EndpointSuggestion("https://api.openai.com/v1"); invalid {
		t.Fatal("ordinary OpenAI /v1 endpoint was incorrectly rejected")
	}
}


func TestGetOpenAIModelInfosIncludesContextWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"big-model","token_limits":{"context_window":1000000}},{"id":"small-model","token_limits":{"context_window":128000}}]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetOpenAIModelInfos: %v", err)
	}
	byID := map[string]int{}
	for _, info := range infos {
		byID[info.ID] = info.ContextWindow
	}
	if byID["big-model"] != 1000000 || byID["small-model"] != 128000 {
		t.Fatalf("context windows = %#v", byID)
	}
	if !protocol.ContextWindowSuggests1M(1000000) || protocol.ContextWindowSuggests1M(128000) {
		t.Fatal("ContextWindowSuggests1M thresholds unexpected")
	}
}


func TestGetOpenAIModelInfosAcceptsTopLevelContextWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"sdk-model","context_window":1000000},{"id":"nested","token_limits":{"context_window":200000}}]}`))
	}))
	t.Cleanup(server.Close)
	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, info := range infos {
		byID[info.ID] = info.ContextWindow
	}
	if byID["sdk-model"] != 1000000 {
		t.Fatalf("top-level context_window not parsed: %#v", byID)
	}
	if byID["nested"] != 200000 {
		t.Fatalf("nested context_window not parsed: %#v", byID)
	}
}
