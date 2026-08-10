package providersession

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestPrepareKeepsDirectAnthropicProviderDirect(t *testing.T) {
	original := provider.Provider{
		Name:     "anthropic-gateway",
		Type:     "anthropic",
		Endpoint: "https://example.test",
		APIKey:   "test-key",
		Model:    "claude-test",
	}

	session, err := Prepare(context.Background(), original)
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	t.Cleanup(session.Close)
	if session.UseProxy || session.Runtime != nil {
		t.Fatalf("direct Anthropic provider started a proxy: %+v", session)
	}
	if session.Provider.Name != original.Name || session.Provider.Type != original.Type ||
		session.Provider.Endpoint != original.Endpoint || session.Provider.APIKey != original.APIKey ||
		session.Provider.Model != original.Model || session.BaseURL != original.Endpoint {
		t.Fatalf("direct provider changed: %+v", session)
	}
}

func TestPrepareDiscoversModelsAndStartsResponsesRuntime(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"}]}`))
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	original := provider.Provider{
		Name:     "responses-gateway",
		Type:     "openai_responses",
		Endpoint: upstream.URL + "/gateway/v1",
		APIKey:   "upstream-key",
	}

	session, err := Prepare(context.Background(), original)
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	t.Cleanup(session.Close)
	if !session.UseProxy || session.Runtime == nil {
		t.Fatal("Responses provider did not start the shared adapter")
	}
	if session.Provider.Model != "gpt-test" {
		t.Fatalf("discovered model = %q", session.Provider.Model)
	}
	if !strings.HasPrefix(session.Provider.Endpoint, "http://127.0.0.1:") || session.BaseURL == original.Endpoint {
		t.Fatalf("runtime endpoint was not resolved: %+v", session)
	}
	if session.Provider.APIKey == "" || session.Provider.APIKey == original.APIKey {
		t.Fatalf("runtime API key was not isolated: %q", session.Provider.APIKey)
	}
	if original.Model != "" || original.Endpoint != upstream.URL+"/gateway/v1" {
		t.Fatalf("Prepare mutated persisted provider: %+v", original)
	}

	// Session owns runtime teardown and Close is intentionally idempotent.
	session.Close()
	session.Close()
}
