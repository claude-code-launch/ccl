package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestFixedOAuthProtocolDefaults(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{oauthproxy.ProviderChatGPT, "openai_responses"},
		{oauthproxy.ProviderGemini, "openai"},
	}
	for _, test := range tests {
		got := fixedOAuthProtocol(test.provider)
		if got != test.want {
			t.Fatalf("fixedOAuthProtocol(%q) = %q; want %q", test.provider, got, test.want)
		}
	}
}

func TestRunAuthIgnoresLegacyProtocolOverrideInConfigMigration(t *testing.T) {
	// Covered by config.Load migration; ensure re-auth always rewrites fixed type.
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "c.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	cfg := &provider.Config{Providers: map[string]provider.Provider{
		"chatgpt": {Name: "chatgpt", Type: "openai", OAuthProvider: "chatgpt", Endpoint: "oauth://codex"},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runAuth(context.Background(), &bytes.Buffer{}, "chatgpt", authOptions{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["chatgpt"].Type != "openai_responses" {
		t.Fatalf("expected fixed responses type, got %+v", loaded.Providers["chatgpt"])
	}
}

func TestRunAuthCreatesChatGPTProviderAndMigratesLegacyCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{
			Provider: target,
			Backend:  "codex",
			Path:     filepath.Join(home, ".ccl", "auth", "codex-test.json"),
		}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	initial := &provider.Config{
		Providers: map[string]provider.Provider{
			"codex": {Name: "codex", OAuthProvider: "codex", EffortLevel: "high", CustomModelID: "gpt-test"},
		},
	}
	if err := config.Save(initial); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	var out bytes.Buffer
	if err := runAuth(context.Background(), &out, "chatgpt", authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["chatgpt"]
	if cfg.ActiveProvider != "chatgpt" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
	if _, exists := cfg.Providers["codex"]; exists {
		t.Fatal("legacy Codex OAuth provider was not removed")
	}
	if p.Type != "openai_responses" || p.Endpoint != "oauth://codex" || p.OAuthProvider != "chatgpt" {
		t.Fatalf("OAuth provider = %+v", p)
	}
	if p.EffortLevel != "high" || p.CustomModelID != "gpt-test" {
		t.Fatalf("existing provider settings were not preserved: %+v", p)
	}
}

func TestRunAuthRejectsPublicCodexAlias(t *testing.T) {
	called := false
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, _ string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		called = true
		return oauthproxy.LoginResult{}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	err := runAuth(context.Background(), &bytes.Buffer{}, "codex", authOptions{})
	if err == nil {
		t.Fatal("runAuth(codex) should fail")
	}
	if called {
		t.Fatal("removed Codex alias invoked the OAuth login flow")
	}
}

func TestRunAuthGeminiUsesChatAndAntigravityBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "antigravity", Path: "credential.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, "gemini", authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["gemini"]
	if p.Type != "openai" || p.Endpoint != "oauth://antigravity" || p.OAuthProvider != "gemini" {
		t.Fatalf("Gemini provider = %+v", p)
	}
}

func TestPrepareProviderRuntimeRejectsAnthropicOAuthProvider(t *testing.T) {
	_, cleanup, err := prepareProviderRuntime(provider.Provider{
		Name:          "chatgpt",
		Type:          "anthropic",
		OAuthProvider: "chatgpt",
	})
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("prepareProviderRuntime() should reject Anthropic OAuth providers")
	}
}

func TestPrepareProviderRuntimeRoutesManualResponsesThroughCodexAdapter(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	original := provider.Provider{
		Name:     "sharedchat",
		Type:     "openai_responses",
		Endpoint: upstream.URL + "/v1/responses",
		APIKey:   "upstream-key",
		Model:    "gpt-5.4-mini",
	}

	runtimeProvider, cleanup, err := prepareProviderRuntime(original)
	if err != nil {
		t.Fatalf("prepareProviderRuntime() error: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(runtimeProvider.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("runtime endpoint = %q", runtimeProvider.Endpoint)
	}
	if runtimeProvider.Endpoint == original.Endpoint {
		t.Fatal("manual Responses provider bypassed the embedded Codex adapter")
	}
	if runtimeProvider.APIKey == "" || runtimeProvider.APIKey == original.APIKey {
		t.Fatalf("runtime API key was not isolated: %q", runtimeProvider.APIKey)
	}
	if original.Endpoint != upstream.URL+"/v1/responses" || original.APIKey != "upstream-key" {
		t.Fatalf("original provider was mutated: %+v", original)
	}
}

func TestPrepareProviderRuntimeRoutesManualChatThroughCLIProxyAPI(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	original := provider.Provider{
		Name:     "openai-chat",
		Type:     "openai",
		Endpoint: upstream.URL + "/v1",
		APIKey:   "upstream-key",
		Model:    "gpt-5.4-mini",
	}

	runtimeProvider, cleanup, err := prepareProviderRuntime(original)
	if err != nil {
		t.Fatalf("prepareProviderRuntime() error: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(runtimeProvider.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("runtime endpoint = %q", runtimeProvider.Endpoint)
	}
	if runtimeProvider.Endpoint == original.Endpoint {
		t.Fatal("manual Chat provider bypassed embedded CLIProxyAPI")
	}
	if runtimeProvider.APIKey == "" || runtimeProvider.APIKey == original.APIKey {
		t.Fatalf("runtime API key was not isolated: %q", runtimeProvider.APIKey)
	}
	if original.Endpoint != upstream.URL+"/v1" || original.APIKey != "upstream-key" {
		t.Fatalf("original provider was mutated: %+v", original)
	}
}

func TestOAuthProviderCanDiscoverModelsForSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credential := []byte(`{"type":"codex","access_token":"test-token","refresh_token":"test-refresh","email":"test@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "codex-set.json"), credential, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	p := provider.Provider{
		Name:          "chatgpt",
		Type:          "openai_responses",
		Endpoint:      "oauth://codex",
		OAuthProvider: "chatgpt",
	}
	runtimeProvider, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		t.Fatalf("prepareProviderRuntime() error: %v", err)
	}
	defer cleanup()

	m := NewAdvancedConfigModel(&p)
	m.configureOAuthRuntime(runtimeProvider.Endpoint, runtimeProvider.APIKey)
	m.detecting = true
	msg, ok := modelFetchCmd(runtimeProvider.Endpoint, runtimeProvider.APIKey)().(modelFetchDoneMsg)
	if !ok {
		t.Fatal("modelFetchCmd() returned an unexpected message type")
	}
	next, _ := m.Update(msg)
	m = next.(*AdvancedConfigModel)

	if m.detectionError != nil || m.page != 5 || p.Model == "" {
		t.Fatalf("OAuth set discovery failed: page=%d models=%q err=%v", m.page, p.Model, m.detectionError)
	}
	if p.Endpoint != "oauth://codex" || p.APIKey != "" || p.Type != "openai_responses" {
		t.Fatalf("OAuth runtime values leaked into stored provider: %+v", p)
	}
}
