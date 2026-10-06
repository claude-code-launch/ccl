package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// newMockOAuthMessagesServer stands in for a subscription's loopback runtime:
// it answers the Anthropic Messages surface with the ccl session key and 404s
// every OpenAI-shaped route. An
// availability probe that follows the persisted compatibility type (openai /
// openai_responses) rather than the surface 404s all of its models.
func newMockOAuthMessagesServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "ccl-local-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestMapAutoUsesDiscoveredOAuthRuntimeCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := newMockOAuthMessagesServer(t)
	cfg := &provider.Config{
		ActiveProvider: "oauth-test",
		Providers: map[string]provider.Provider{
			"oauth-test": {
				Name: "oauth-test", Type: "anthropic", Endpoint: "oauth://qoder",
				OAuthProvider: "qoder", OAuthAccountCredential: "qoder-user.json",
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	original := fetchMappingCatalog
	var cleanupCalls atomic.Int32
	fetchMappingCatalog = func(_ context.Context, p provider.Provider) (provider.Provider, []string, map[string]protocol.ModelInfo, func(), error) {
		if p.OAuthProvider != "qoder" || p.Endpoint != "oauth://qoder" {
			t.Fatalf("mapping catalog input = %+v", p)
		}
		// A subscription's compatibility type is only a local-dispatch value; the
		// surface it serves is Anthropic Messages, exactly as the loopback runtime
		// does. Kimi/Gemini/WorkBuddy persist "openai" here.
		p.Type = "openai"
		p.Endpoint = server.URL + "/v1"
		p.APIKey = "ccl-local-key"
		return p, []string{"model-a", "model-b", "model-c", "model-d"}, nil, func() {
			cleanupCalls.Add(1)
		}, nil
	}
	t.Cleanup(func() { fetchMappingCatalog = original })

	if err := runMapAuto(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if cleanupCalls.Load() != 1 {
		t.Fatalf("runtime cleanup calls = %d", cleanupCalls.Load())
	}
	updated, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := updated.Providers["oauth-test"]
	if p.Model != "model-a,model-b,model-c,model-d" {
		t.Fatalf("saved model pool = %q", p.Model)
	}
	if strings.Join([]string{p.OpusModel, p.SonnetModel, p.HaikuModel, p.CustomModelID}, ",") != "model-a,model-b,model-c,model-d" {
		t.Fatalf("slot mapping = %+v", p)
	}
	if p.Endpoint != "oauth://qoder" || p.APIKey != "" {
		t.Fatalf("ephemeral runtime leaked into config: %+v", p)
	}
}

func TestMappingCatalogErrorNoLongerRequiresSet(t *testing.T) {
	original := fetchMappingCatalog
	fetchMappingCatalog = func(_ context.Context, p provider.Provider) (provider.Provider, []string, map[string]protocol.ModelInfo, func(), error) {
		return p, nil, nil, func() {}, nil
	}
	t.Cleanup(func() { fetchMappingCatalog = original })

	p := provider.Provider{OAuthProvider: "qoder", Endpoint: "oauth://qoder"}
	_, err := modelPoolForMapping(context.Background(), p)
	if err == nil {
		t.Fatal("expected an empty catalog error")
	}
	if strings.Contains(err.Error(), "ccl set") {
		t.Fatal("mapping errors must not instruct OAuth users to run ccl set")
	}
}
