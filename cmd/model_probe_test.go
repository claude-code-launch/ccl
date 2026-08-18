package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
