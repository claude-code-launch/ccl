package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// newMockGatewayServer simulates an OpenAI-compatible gateway. Anthropic-shaped
// requests (identified by the x-api-key header) to /v1/models always fail with
// 401, simulating a non-Anthropic backend. OpenAI-shaped requests
// (Authorization: Bearer, no x-api-key) succeed with the given models.
// Requests to /v1/responses succeed only when responsesSupported is true.
func newMockGatewayServer(t *testing.T, models []string, responsesSupported bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var data []map[string]string
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !responsesSupported {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestDetectProtocolAndModelsDetectsAnthropic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-5-sonnet","type":"model"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", proto)
	}
	if models != "claude-3-5-sonnet" {
		t.Errorf("expected models 'claude-3-5-sonnet', got %q", models)
	}
}

func TestDetectProtocolAndModelsDetectsAnthropicBearerModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-6.7-flash-lite","type":"model"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"sensenova-6.7-flash-lite","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL, "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", result.protocol)
	}
	if result.anthropicAuth != anthropicAuthBearer {
		t.Errorf("expected anthropic auth %q, got %q", anthropicAuthBearer, result.anthropicAuth)
	}
	if result.models != "sensenova-6.7-flash-lite" {
		t.Errorf("expected models 'sensenova-6.7-flash-lite', got %q", result.models)
	}
}

func TestFetchModelsForProviderUsesAnthropicBearerAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-u1-fast"},{"id":"sensenova-6.7-flash-lite"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	models := fetchModelsForProvider(provider.Provider{
		Type:          "anthropic",
		Endpoint:      server.URL,
		APIKey:        "test-key",
		AnthropicAuth: anthropicAuthBearer,
	})

	if got, want := strings.Join(models, ","), "sensenova-u1-fast,sensenova-6.7-flash-lite"; got != want {
		t.Fatalf("expected models %q, got %q", want, got)
	}
}

func TestApplyModelDetectionResultNormalizesAnthropicEndpoint(t *testing.T) {
	p := provider.Provider{Endpoint: "https://token.sensenova.cn/v1"}
	m := NewAdvancedConfigModel(&p)

	_ = m.applyModelDetectionResult("anthropic", "sensenova-u1-fast", anthropicAuthBearer, nil)

	if p.Endpoint != "https://token.sensenova.cn" {
		t.Fatalf("expected endpoint without /v1, got %q", p.Endpoint)
	}
	if p.AnthropicAuth != anthropicAuthBearer {
		t.Fatalf("expected bearer auth, got %q", p.AnthropicAuth)
	}
	if p.Model != "sensenova-u1-fast" {
		t.Fatalf("expected detected model pool to be saved, got %q", p.Model)
	}
}

func TestDetectProtocolAndModelsDefaultsOpenAIToChatEvenWhenResponsesWorks(t *testing.T) {
	server := newMockGatewayServer(t, []string{"gpt-5"}, true)

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Errorf("expected protocol 'openai' (openai(chat)), got %q", proto)
	}
	if models != "gpt-5" {
		t.Errorf("expected models 'gpt-5', got %q", models)
	}
}

func TestDetectProtocolAndModelsDoesNotTreatBearerModelsAsAnthropicWithoutMessages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Fatalf("expected OpenAI Chat, got %q", proto)
	}
	if models != "gpt-4o" {
		t.Fatalf("expected OpenAI models to be kept, got %q", models)
	}
}

func TestDetectProtocolAndModelsDetectsAnthropicMessagesWithoutAnthropicModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") == "" {
			http.Error(w, "missing version", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"gpt-5.5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", proto)
	}
	if models != "gpt-5.5" {
		t.Errorf("expected models 'gpt-5.5', got %q", models)
	}
}

func TestDetectProtocolAndModelsDetectsAnthropicBearerMessagesWithoutAnthropicModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-6.7-flash-lite"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"sensenova-6.7-flash-lite","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL+"/v1", "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", result.protocol)
	}
	if result.anthropicAuth != anthropicAuthBearer {
		t.Errorf("expected anthropic auth %q, got %q", anthropicAuthBearer, result.anthropicAuth)
	}
	if result.models != "sensenova-6.7-flash-lite" {
		t.Errorf("expected models 'sensenova-6.7-flash-lite', got %q", result.models)
	}
}

func TestDetectProtocolAndModelsFallsBackToOpenAIChat(t *testing.T) {
	server := newMockGatewayServer(t, []string{"gpt-4o"}, false)

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Errorf("expected protocol 'openai' (openai(chat)), got %q", proto)
	}
	if models != "gpt-4o" {
		t.Errorf("expected models 'gpt-4o', got %q", models)
	}
}

func TestDetectProtocolAndModelsBothFail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "bad-key")
	if err == nil {
		t.Fatalf("expected error when both protocols fail")
	}
	if models != "" {
		t.Errorf("expected empty models on failure, got %q", models)
	}
	if proto != "" {
		t.Errorf("expected empty protocol on failure, got %q", proto)
	}
	if !strings.Contains(err.Error(), "unsupported protocol") && !strings.Contains(err.Error(), "暂不支持这个协议") {
		t.Errorf("expected unsupported protocol error, got %q", err.Error())
	}
}

func TestDetectProtocolAndModelsRejectsModelsOnlyGateway(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err == nil {
		t.Fatalf("expected error for models-only gateway")
	}
	if proto != "" || models != "" {
		t.Fatalf("expected empty protocol/models, got proto=%q models=%q", proto, models)
	}
}
