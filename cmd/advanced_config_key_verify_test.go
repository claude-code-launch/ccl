package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestVerifyProviderAPIKeyRejectsAuthFailure proves the core fix for the
// models.dev "Test Connection" flow: a fake/revoked key that reaches the auth
// layer (401/403) must report "invalid key", NOT "connected". This is what
// distinguishes the real inference probe from the old /models listing probe,
// which returns err==nil on public-model-list gateways regardless of the key.
func TestVerifyProviderAPIKeyRejectsAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", status)
		}))
		defer server.Close()

		for _, proto := range []string{"anthropic", "openai_responses", "openai"} {
			err := verifyProviderAPIKey(t.Context(), "test-model", server.URL, "fake-key", proto, 2*time.Second)
			if err == nil {
				t.Errorf("proto=%s status=%d: expected invalid-key error, got nil", proto, status)
			}
		}
	}
}

// TestVerifyProviderAPIKeyAcceptsValidKey proves a 2xx (and any non-auth 4xx
// such as bad model/params/rate-limit) means the key passed authentication.
func TestVerifyProviderAPIKeyAcceptsValidKey(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusOK {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			http.Error(w, "nope", status)
		}))
		defer server.Close()

		for _, proto := range []string{"anthropic", "openai_responses", "openai"} {
			if err := verifyProviderAPIKey(t.Context(), "test-model", server.URL, "good-key", proto, 2*time.Second); err != nil {
				t.Errorf("proto=%s status=%d: expected nil (key passed auth), got %v", proto, status, err)
			}
		}
	}
}

// TestVerifyProviderAPIKeyFailsOnServerError proves 5xx is reported as
// "cannot verify" rather than a false "connected".
func TestVerifyProviderAPIKeyFailsOnServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	for _, proto := range []string{"anthropic", "openai_responses", "openai"} {
		if err := verifyProviderAPIKey(t.Context(), "test-model", server.URL, "key", proto, 2*time.Second); err == nil {
			t.Errorf("proto=%s: expected 5xx to fail verification, got nil", proto)
		}
	}
}

// TestVerifyProviderAPIKeyFailsOnTransportError proves an unreachable endpoint
// cannot be reported as "connected".
func TestVerifyProviderAPIKeyFailsOnTransportError(t *testing.T) {
	for _, proto := range []string{"anthropic", "openai_responses", "openai"} {
		if err := verifyProviderAPIKey(t.Context(), "test-model", "http://127.0.0.1:1", "key", proto, 500*time.Millisecond); err == nil {
			t.Errorf("proto=%s: expected transport error to fail verification, got nil", proto)
		}
	}
}

// TestVerifyProviderAPIKeyAnthropicUsesMessagesPath guards the URL normalization
// so the anthropic branch actually hits /v1/messages (not /models), which is
// where auth is enforced.
func TestVerifyProviderAPIKeyAnthropicUsesMessagesPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("x-api-key"); got != "key-123" {
			t.Errorf("x-api-key = %q", got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	if err := verifyProviderAPIKey(t.Context(), "claude-x", server.URL, "key-123", "anthropic", 2*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("anthropic branch path = %q, want /v1/messages", gotPath)
	}
}

// TestVerifyProviderAPIKeyOpenAIResponsesUsesResponsesPath guards that the
// responses branch posts to /responses and sends the model in the body.
func TestVerifyProviderAPIKeyOpenAIResponsesUsesResponsesPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	if err := verifyProviderAPIKey(t.Context(), "gpt-x", server.URL, "key-123", "openai_responses", 2*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/responses" {
		t.Errorf("responses branch path = %q, want /responses", gotPath)
	}
}

// TestVerifyProviderAPIKeyOpenAIChatUsesCompletionsPath guards the chat branch.
func TestVerifyProviderAPIKeyOpenAIChatUsesCompletionsPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("Authorization"); got != "Bearer key-123" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	if err := verifyProviderAPIKey(t.Context(), "gpt-x", server.URL, "key-123", "openai", 2*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("chat branch path = %q, want /chat/completions", gotPath)
	}
}

// TestFirstRoutableModel guards the empty-model-pool / unknown-protocol case:
// when no model in the pool has an entry in the per-model protocol table (e.g.
// an unrecognized AI SDK npm package), firstRoutableModel must return ok=false
// so the UI reports "no usable model" instead of fabricating a connection.
func TestFirstRoutableModel(t *testing.T) {
	newModel := func(pool []string, protocols map[string]string) *AdvancedConfigModel {
		p := &provider.Provider{ModelProtocols: protocols}
		draft := &connDraft{p: p, modelPool: pool}
		return &AdvancedConfigModel{
			source:         sourceCustom,
			customDraft:    draft,
			modelsDevDraft: &connDraft{p: &provider.Provider{}},
			p:              p,
		}
	}

	t.Run("first model with a known protocol", func(t *testing.T) {
		m := newModel([]string{"gpt-x", "claude-y"}, map[string]string{
			"gpt-x":    "openai_responses",
			"claude-y": "anthropic",
		})
		model, proto, ok := m.firstRoutableModel()
		if !ok || model != "gpt-x" || proto != "openai_responses" {
			t.Fatalf("firstRoutableModel = (%q, %q, %t), want (gpt-x, openai_responses, true)", model, proto, ok)
		}
	})

	t.Run("empty pool", func(t *testing.T) {
		m := newModel(nil, map[string]string{"gpt-x": "openai_responses"})
		if _, _, ok := m.firstRoutableModel(); ok {
			t.Fatal("empty pool should return ok=false")
		}
	})

	t.Run("no model has a protocol", func(t *testing.T) {
		m := newModel([]string{"gpt-x"}, map[string]string{})
		if _, _, ok := m.firstRoutableModel(); ok {
			t.Fatal("pool with no protocol entries should return ok=false")
		}
	})
}
