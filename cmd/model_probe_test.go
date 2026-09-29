package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClinePassAvailabilityProbeUsesDocumentedRequestShape(t *testing.T) {
	const model = "cline-pass/qwen3.8-max"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization header missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["model"] != model || body["stream"] != false {
			t.Errorf("model/stream = %v/%v", body["model"], body["stream"])
		}
		if _, ok := body["max_tokens"]; ok {
			t.Error("ClinePass probe should not send max_tokens")
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Error("ClinePass probe should not send max_completion_tokens")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if got := probeModelAvailability(context.Background(), model, server.URL+"/api/v1", "test-key", "modelsdev", "", map[string]string{model: "openai"}, true); got != modelAvailabilityAvailable {
		t.Fatalf("ClinePass probe = %v, want available", got)
	}
}

func TestAvailabilityProbeDistinguishesUnavailableFromInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		keyVerified bool
		want        modelAvailability
	}{
		{"bad probe request", http.StatusBadRequest, true, modelAvailabilityInconclusive},
		{"not found without verified key", http.StatusNotFound, false, modelAvailabilityInconclusive},
		{"auth failure", http.StatusUnauthorized, true, modelAvailabilityInconclusive},
		{"not found with verified key", http.StatusNotFound, true, modelAvailabilityUnavailable},
		{"conflict with verified key", http.StatusConflict, true, modelAvailabilityInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			got := probeModelAvailability(context.Background(), "cline-pass/test", server.URL+"/v1", "key", "modelsdev", "", nil, tc.keyVerified)
			if got != tc.want {
				t.Fatalf("availability = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAvailabilityProbeRetriesRateLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	got := probeModelAvailability(context.Background(), "cline-pass/test", server.URL+"/v1", "key", "modelsdev", "", nil, true)
	if got != modelAvailabilityAvailable || requests.Load() != 2 {
		t.Fatalf("availability = %v after %d requests, want available after 2", got, requests.Load())
	}
}

// mixedProtocolStub stands in for a models.dev gateway: each wire endpoint only
// accepts the model declared for that protocol, so a probe that picks the wrong
// wire protocol gets a non-2xx.
func mixedProtocolStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var model string
		if request.Body != nil {
			buf := make([]byte, 4096)
			n, _ := request.Body.Read(buf)
			model = gjsonModelValue(buf[:n])
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/chat/completions"):
			if model == "chat-model" {
				writer.WriteHeader(http.StatusOK)
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
		case strings.HasSuffix(request.URL.Path, "/responses"):
			if model == "resp-model" {
				writer.WriteHeader(http.StatusOK)
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
		case strings.HasSuffix(request.URL.Path, "/messages"):
			if model == "anthropic-model" {
				writer.WriteHeader(http.StatusOK)
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}

func gjsonModelValue(body []byte) string {
	idx := strings.Index(string(body), `"model":"`)
	if idx < 0 {
		return ""
	}
	rest := string(body[idx+len(`"model":"`):])
	if end := strings.Index(rest, `"`); end >= 0 {
		return rest[:end]
	}
	return ""
}

// TestResponsesProbeSurfacesFailureStatus guards the Responses branch of
// probeSingleModelForProtocolStatusContext: a gateway that rejects the model
// (404) must surface that status so probeModelAvailability can mark it
// unavailable on a verified key, not swallow it as inconclusive.
func TestResponsesProbeSurfacesFailureStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("path = %q, want a /responses endpoint", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	status, err := probeSingleModelForProtocolStatusContext(context.Background(), "gone-model", server.URL+"/v1", "key", "openai_responses", "", 5*time.Second)
	if err != nil {
		t.Fatalf("responses probe err = %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("responses probe status = %d, want 404", status)
	}
	if got := probeModelAvailability(context.Background(), "gone-model", server.URL+"/v1", "key", "modelsdev", "", map[string]string{"gone-model": "openai_responses"}, true); got != modelAvailabilityUnavailable {
		t.Fatalf("responses 404 with verified key = %v, want unavailable", got)
	}
}

// TestModelsDevProbeRoutesPerModelProtocol verifies a mixed-protocol gateway's
// models are probed over their declared wire protocol. Probing them all as Chat
// Completions (the old behavior) marks the Responses and Anthropic models
// unavailable even though they work.
func TestModelsDevProbeRoutesPerModelProtocol(t *testing.T) {
	stub := mixedProtocolStub(t)
	defer stub.Close()

	protocols := map[string]string{
		"chat-model":      "openai",
		"resp-model":      "openai_responses",
		"anthropic-model": "anthropic",
	}
	endpoint := stub.URL + "/v1"
	for _, model := range []string{"chat-model", "resp-model", "anthropic-model"} {
		if !testSingleModelWithProtocolsContext(context.Background(), model, endpoint, "key", "modelsdev", "", protocols, 5*time.Second) {
			t.Fatalf("model %s probed unavailable despite matching its declared protocol", model)
		}
	}

	// The old single-protocol behavior really is wrong for these models: the
	// same models probed as plain chat fail.
	for _, model := range []string{"resp-model", "anthropic-model"} {
		if testSingleModelForProtocolContext(context.Background(), model, endpoint, "key", "openai", "", 5*time.Second) {
			t.Fatalf("model %s unexpectedly available over the wrong protocol", model)
		}
	}
}

// TestProbeStripsOneMSuffix verifies the [1m] context marker is not sent
// upstream as part of the model name.
func TestProbeStripsOneMSuffix(t *testing.T) {
	var probed atomic.Value
	stub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		buf := make([]byte, 4096)
		n, _ := request.Body.Read(buf)
		probed.Store(gjsonModelValue(buf[:n]))
		writer.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	if !testSingleModelForProtocolContext(context.Background(), "foo[1m]", stub.URL+"/v1", "key", "openai", "", 5*time.Second) {
		t.Fatal("probe with [1m] marker failed")
	}
	if got := probed.Load().(string); got != "foo" {
		t.Fatalf("upstream saw model %q, want foo", got)
	}
}

// TestChatProbeRetriesWithMaxCompletionTokens covers gateways that reject the
// legacy max_tokens parameter for reasoning-model families.
func TestChatProbeRetriesWithMaxCompletionTokens(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		buf := make([]byte, 4096)
		n, _ := request.Body.Read(buf)
		body := string(buf[:n])
		if strings.Contains(body, `"max_tokens"`) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":{"message":"Unsupported parameter: max_tokens"}}`))
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	if !testSingleOpenAIModelContext(context.Background(), "some-reasoning-model", stub.URL+"/v1", "key", 5*time.Second) {
		t.Fatal("chat probe did not retry with max_completion_tokens after a 400 on max_tokens")
	}
}
