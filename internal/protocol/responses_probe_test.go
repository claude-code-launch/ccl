package protocol_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestProbeOpenAIResponsesSupportSucceedsOn2xx(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("missing/incorrect authorization header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer server.Close()

	ok := protocol.ProbeOpenAIResponsesSupport(server.URL+"/v1", "test-key", "gpt-5", 2*time.Second)
	if !ok {
		t.Fatalf("expected ProbeOpenAIResponsesSupport to succeed")
	}
	if gotBody["model"] != "gpt-5" {
		t.Errorf("expected model 'gpt-5' in request body, got %v", gotBody["model"])
	}
	if store, ok := gotBody["store"].(bool); !ok || store {
		t.Errorf("expected store=false in request body, got %v", gotBody["store"])
	}
}

func TestProbeOpenAIResponsesSupportFailsOnNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	if protocol.ProbeOpenAIResponsesSupport(server.URL+"/v1", "test-key", "gpt-5", 2*time.Second) {
		t.Fatalf("expected ProbeOpenAIResponsesSupport to fail on 404")
	}
}

func TestProbeOpenAIResponsesSupportFailsOnUnreachable(t *testing.T) {
	if protocol.ProbeOpenAIResponsesSupport("http://127.0.0.1:1", "test-key", "gpt-5", 500*time.Millisecond) {
		t.Fatalf("expected ProbeOpenAIResponsesSupport to fail when endpoint is unreachable")
	}
}
