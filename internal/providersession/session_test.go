package providersession

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestPrepareStartsAutoClawRuntime pins the AutoClaw contract: the remote
// bearer credential remains inside a CCL-owned runtime and Claude Code receives
// only an ephemeral loopback endpoint/key.
func TestPrepareStartsAutoClawRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "autoclaw-account.json"),
		[]byte(`{"type":"autoclaw","api_key":"plan-key.secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := Prepare(context.Background(), provider.Provider{
		Name:                   "autoclaw",
		Type:                   "autoclaw",
		Endpoint:               "https://zcode.z.ai/api/v1/zcode-plan/anthropic",
		OAuthProvider:          "autoclaw",
		OAuthAccountCredential: "autoclaw-account.json",
		Model:                  "GLM-5.3,GLM-5.3-Flash,GLM-5-Turbo",
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	t.Cleanup(session.Close)
	if !session.UseProxy || session.Runtime == nil {
		t.Fatalf("AutoClaw provider did not start a runtime: %+v", session)
	}
	if !strings.HasPrefix(session.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("base URL = %q", session.BaseURL)
	}
	if session.Provider.APIKey == "" || session.Provider.APIKey == "plan-key.secret" {
		t.Fatalf("runtime API key = %q", session.Provider.APIKey)
	}
	if !strings.HasPrefix(session.Provider.Endpoint, "http://127.0.0.1:") || !strings.HasSuffix(session.Provider.Endpoint, "/v1") {
		t.Fatalf("runtime endpoint = %q", session.Provider.Endpoint)
	}
}

// TestPrepareAutoClawReportsMissingCredential keeps the failure actionable.
func TestPrepareAutoClawReportsMissingCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Prepare(context.Background(), provider.Provider{
		Name:                   "autoclaw",
		Type:                   "autoclaw",
		Endpoint:               "https://zcode.z.ai/api/v1/zcode-plan/anthropic",
		OAuthProvider:          "autoclaw",
		OAuthAccountCredential: "missing.json",
	})
	if err == nil {
		t.Fatal("Prepare() must fail when the bound credential is missing")
	}
	if !strings.Contains(err.Error(), "ccl oauth autoclaw") {
		t.Fatalf("error = %v", err)
	}
}
