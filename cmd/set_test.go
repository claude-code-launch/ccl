package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func TestDetectProtocolAndModelsDetectsOpenAIAgent(t *testing.T) {
	server := newMockGatewayServer(t, []string{"gpt-5"}, true)

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai_responses" {
		t.Errorf("expected protocol 'openai_responses' (openai(agent)), got %q", proto)
	}
	if models != "gpt-5" {
		t.Errorf("expected models 'gpt-5', got %q", models)
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
	if proto != "openai" {
		t.Errorf("expected fallback guess 'openai', got %q", proto)
	}
}

func TestDetectOpenAIVariantPrefersResponsesWhenAnyModelSupportsIt(t *testing.T) {
	var responsesCalls int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&responsesCalls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "model-c" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	got := detectOpenAIVariant(server.URL+"/v1", "test-key", "model-a,model-b,model-c,model-d")
	if got != "openai_responses" {
		t.Fatalf("expected 'openai_responses', got %q", got)
	}
	if atomic.LoadInt64(&responsesCalls) == 0 {
		t.Fatalf("expected at least one probe call")
	}
}

func TestDetectOpenAIVariantFallsBackWhenNoModelSupportsResponses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	got := detectOpenAIVariant(server.URL+"/v1", "test-key", "model-a,model-b")
	if got != "openai" {
		t.Fatalf("expected 'openai', got %q", got)
	}
}

func TestDetectOpenAIVariantCapsProbeCandidates(t *testing.T) {
	var calledModels sync.Map
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if model, ok := body["model"].(string); ok {
			calledModels.Store(model, true)
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	models := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"}
	if got := detectOpenAIVariant(server.URL+"/v1", "test-key", strings.Join(models, ",")); got != "openai" {
		t.Fatalf("expected 'openai', got %q", got)
	}

	count := 0
	calledModels.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != maxOpenAIResponsesProbeCandidates {
		t.Fatalf("expected exactly %d probe calls, got %d", maxOpenAIResponsesProbeCandidates, count)
	}
}
