package protocol_test

import (
	"context"
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

func TestProbeOpenAIResponsesSupportDrainsResponseBody(t *testing.T) {
	headersSent := make(chan struct{})
	finishBody := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		close(headersSent)
		<-finishBody
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result := make(chan bool, 1)
	go func() {
		result <- protocol.ProbeOpenAIResponsesSupport(server.URL+"/v1", "test-key", "gpt-5", 2*time.Second)
	}()

	<-headersSent
	select {
	case <-result:
		t.Fatal("probe returned before the streaming response body completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(finishBody)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("expected probe to succeed after draining the response")
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not return after the response body completed")
	}
}

func TestProbeOpenAIResponsesSupportFailsOnUnreachable(t *testing.T) {
	if protocol.ProbeOpenAIResponsesSupport("http://127.0.0.1:1", "test-key", "gpt-5", 500*time.Millisecond) {
		t.Fatalf("expected ProbeOpenAIResponsesSupport to fail when endpoint is unreachable")
	}
}

func TestProbeOpenAIResponsesSupportContextCanBeCanceled(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		result <- protocol.ProbeOpenAIResponsesSupportContext(ctx, server.URL+"/v1", "test-key", "gpt-5", 10*time.Second)
	}()

	<-started
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("expected canceled probe to fail")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled probe did not return promptly")
	}
}
