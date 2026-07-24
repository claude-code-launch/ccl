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
		{oauthproxy.ProviderGrok, "openai"},
		{oauthproxy.ProviderKimi, "openai"},
		{oauthproxy.ProviderClaude, "anthropic"},
		{oauthproxy.ProviderCopilot, "openai_responses"},
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
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-alice@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gpt"}, authOptions{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := loaded.Providers["gpt-alice@example.com"]
	if !ok {
		t.Fatalf("expected derived provider gpt-alice@example.com, got %+v", loaded.Providers)
	}
	if p.Type != "openai_responses" {
		t.Fatalf("expected fixed responses type, got %+v", p)
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
	if err := runAuth(context.Background(), &out, []string{"gpt"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["gpt-test"]
	if cfg.ActiveProvider != "gpt-test" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
	if _, exists := cfg.Providers["codex"]; exists {
		t.Fatal("legacy Codex OAuth provider was not removed")
	}
	if p.Type != "openai_responses" || p.Endpoint != "oauth://codex" || p.OAuthProvider != "gpt" {
		t.Fatalf("OAuth provider = %+v", p)
	}
	if p.OAuthAccountCredential != "codex-test.json" {
		t.Fatalf("credential binding = %q, want codex-test.json", p.OAuthAccountCredential)
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

	err := runAuth(context.Background(), &bytes.Buffer{}, []string{"codex"}, authOptions{})
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
		return oauthproxy.LoginResult{Provider: target, Backend: "antigravity", Path: "antigravity-credential.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gemini"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["gemini-credential"]
	if !ok {
		t.Fatalf("no derived gemini-credential provider: %+v", cfg.Providers)
	}
	if p.Type != "openai" || p.Endpoint != "oauth://antigravity" || p.OAuthProvider != "gemini" {
		t.Fatalf("Gemini provider = %+v", p)
	}
	if p.CustomModelID != "claude-opus-4-6-thinking" || p.OpusModel != "claude-opus-4-6-thinking" ||
		p.SonnetModel != "claude-sonnet-4-6" || p.HaikuModel != "gemini-3.1-pro-low" {
		t.Fatalf("Gemini preferred defaults not applied: %+v", p)
	}
}

func TestRunAuthGeminiPreservesExistingSlotPins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "antigravity", Path: "antigravity-credential.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := config.Save(&provider.Config{
		Providers: map[string]provider.Provider{
			"work": {
				Name:          "work",
				OAuthProvider: "gemini",
				SonnetModel:   "my-sonnet",
			},
		},
	}); err != nil {
		t.Fatalf("save initial: %v", err)
	}
	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gemini", "work"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["work"]
	if p.SonnetModel != "my-sonnet" {
		t.Fatalf("existing sonnet overwritten: %+v", p)
	}
	if p.CustomModelID != "claude-opus-4-6-thinking" || p.OpusModel != "claude-opus-4-6-thinking" || p.HaikuModel != "gemini-3.1-pro-low" {
		t.Fatalf("empty gemini slots not filled: %+v", p)
	}
}

func TestRunAuthAliasBindsToCredentialAndSetsActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-work@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gpt", "work"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["work"]
	if !ok {
		t.Fatalf("no aliased provider work: %+v", cfg.Providers)
	}
	if cfg.ActiveProvider != "work" {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
	if p.OAuthAccountCredential != "codex-work@example.com.json" {
		t.Fatalf("credential = %q", p.OAuthAccountCredential)
	}
}

func TestRunAuthGrokUsesXaiBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "xai", Path: "xai-bob@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"grok", "personal"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["personal"]
	if !ok {
		t.Fatalf("no grok provider personal: %+v", cfg.Providers)
	}
	if p.Type != "openai" || p.Endpoint != "oauth://xai" || p.OAuthProvider != "grok" {
		t.Fatalf("Grok provider = %+v", p)
	}
	if p.OAuthAccountCredential != "xai-bob@example.com.json" {
		t.Fatalf("credential = %q", p.OAuthAccountCredential)
	}
	if p.CustomModelID != "grok-4.5" || p.OpusModel != "grok-4.5" || p.SonnetModel != "grok-4.3" || p.HaikuModel != "grok-3-mini" {
		t.Fatalf("Grok preferred defaults not applied: %+v", p)
	}
}

func TestRunAuthGrokPreservesExistingSlotPins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "xai", Path: "xai-bob@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := config.Save(&provider.Config{
		Providers: map[string]provider.Provider{
			"personal": {
				Name:          "personal",
				OAuthProvider: "grok",
				OpusModel:     "my-opus",
				SonnetModel:   "my-sonnet",
			},
		},
	}); err != nil {
		t.Fatalf("save initial: %v", err)
	}
	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"grok", "personal"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["personal"]
	if p.OpusModel != "my-opus" || p.SonnetModel != "my-sonnet" {
		t.Fatalf("existing pins overwritten: %+v", p)
	}
	if p.CustomModelID != "grok-4.5" || p.HaikuModel != "grok-3-mini" {
		t.Fatalf("empty slots not filled with defaults: %+v", p)
	}
}

func TestRunAuthCopilotUsesCodexResponsesBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		if target != oauthproxy.ProviderCopilot {
			t.Fatalf("login target = %q, want copilot", target)
		}
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-copilot@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"copilot", "gh"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["gh"]
	if !ok {
		t.Fatalf("no copilot provider gh: %+v", cfg.Providers)
	}
	if p.Type != "openai_responses" || p.Endpoint != "oauth://codex" || p.OAuthProvider != "copilot" {
		t.Fatalf("Copilot provider = %+v", p)
	}
}

func TestRunAuthGrokWithoutAliasDerivesName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "xai", Path: "xai-work@x.ai.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"grok"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	derived := "grok-work@x.ai"
	p, ok := cfg.Providers[derived]
	if !ok {
		t.Fatalf("no derived grok provider %q: %+v", derived, cfg.Providers)
	}
	if p.OAuthProvider != "grok" {
		t.Fatalf("Grok provider = %+v", p)
	}
}

func TestRunAuthCopilotWithoutAliasDerivesName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-copilot@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"copilot"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	derived := "copilot-copilot@example.com"
	p, ok := cfg.Providers[derived]
	if !ok {
		t.Fatalf("no derived copilot provider %q: %+v", derived, cfg.Providers)
	}
	if p.OAuthProvider != "copilot" {
		t.Fatalf("Copilot provider = %+v", p)
	}
}

func TestRunAuthRejectsReservedAlias(t *testing.T) {
	originalLogin := oauthLogin
	oauthLogin = func(context.Context, string, oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gpt", "grok"}, authOptions{}); err == nil {
		t.Fatal("runAuth(gpt grok) should reject reserved alias")
	}
}

func TestRunAuthKimiUsesOpenAIChatBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "kimi", Path: "kimi-123.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"kimi", "moon"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["moon"]
	if !ok {
		t.Fatalf("no kimi provider moon: %+v", cfg.Providers)
	}
	if p.Type != "openai" || p.Endpoint != "oauth://kimi" || p.OAuthProvider != "kimi" {
		t.Fatalf("Kimi provider = %+v", p)
	}
	if p.OAuthAccountCredential != "kimi-123.json" {
		t.Fatalf("credential = %q", p.OAuthAccountCredential)
	}
}

func TestRunAuthKimiWithoutAliasDerivesName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "kimi", Path: "kimi-1712345678.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"kimi"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["kimi-1712345678"]
	if !ok {
		t.Fatalf("no derived kimi provider: %+v", cfg.Providers)
	}
	if p.OAuthProvider != "kimi" {
		t.Fatalf("Kimi provider = %+v", p)
	}
}

func TestRunAuthClaudeUsesAnthropicBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "claude", Path: "claude-alice@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"claude", "anthropic-acct"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["anthropic-acct"]
	if !ok {
		t.Fatalf("no claude provider anthropic-acct: %+v", cfg.Providers)
	}
	if p.Type != "anthropic" || p.Endpoint != "oauth://claude" || p.OAuthProvider != "claude" {
		t.Fatalf("Claude provider = %+v", p)
	}
	if p.OAuthAccountCredential != "claude-alice@example.com.json" {
		t.Fatalf("credential = %q", p.OAuthAccountCredential)
	}
}

func TestRunAuthClaudeWithoutAliasDerivesName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "claude", Path: "claude-alice@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"claude"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	derived := "claude-alice@example.com"
	p, ok := cfg.Providers[derived]
	if !ok {
		t.Fatalf("no derived claude provider %q: %+v", derived, cfg.Providers)
	}
	if p.OAuthProvider != "claude" || p.Type != "anthropic" {
		t.Fatalf("Claude provider = %+v", p)
	}
}

func TestRunAuthPreservesFastModeOnReauth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-work@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := config.Save(&provider.Config{
		Providers: map[string]provider.Provider{
			"work": {
				Name:          "work",
				OAuthProvider: "gpt",
				Endpoint:      "oauth://codex",
				FastMode:      true,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Re-auth never rewrites FastMode; only the Claude Code /fast toggle
	// or ccl set Review & Apply does.
	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gpt", "work"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["work"].FastMode {
		t.Fatalf("FastMode cleared on re-auth: %+v", cfg.Providers["work"])
	}
}

func TestProviderFastSummary(t *testing.T) {
	if got := providerFastSummary(provider.Provider{FastMode: true, OAuthProvider: "gpt"}); got != "on" {
		t.Fatalf("gpt fast = %q, want on", got)
	}
	if got := providerFastSummary(provider.Provider{FastMode: true, OAuthProvider: "copilot"}); got != "on" {
		t.Fatalf("copilot fast = %q, want on", got)
	}
	if got := providerFastSummary(provider.Provider{FastMode: true, OAuthProvider: "gemini"}); got != "off" {
		t.Fatalf("gemini fast = %q, want off", got)
	}
	if got := providerFastSummary(provider.Provider{FastMode: false, OAuthProvider: "gpt"}); got != "off" {
		t.Fatalf("gpt off = %q, want off", got)
	}
}

func TestPrepareProviderRuntimeStartsClaudeOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir auth: %v", err)
	}
	// Minimal Claude OAuth credential so the embedded runtime can start.
	cred := []byte(`{"type":"claude","access_token":"test-token","refresh_token":"test-refresh","email":"alice@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "claude-alice@example.com.json"), cred, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	runtimeProvider, cleanup, err := prepareProviderRuntime(provider.Provider{
		Name:                   "anthropic-acct",
		Type:                   "anthropic",
		OAuthProvider:          "claude",
		OAuthAccountCredential: "claude-alice@example.com.json",
		Endpoint:               "oauth://claude",
	})
	if err != nil {
		t.Fatalf("prepareProviderRuntime() error: %v", err)
	}
	t.Cleanup(cleanup)

	if !strings.HasPrefix(runtimeProvider.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("runtime endpoint = %q", runtimeProvider.Endpoint)
	}
	if runtimeProvider.APIKey == "" {
		t.Fatal("runtime API key empty")
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
		OAuthProvider: "gpt",
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

func TestRunAuthGPTAppliesPreferredDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-alice@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"gpt", "main"}, authOptions{}); err != nil {
		t.Fatalf("runAuth() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, ok := cfg.Providers["main"]
	if !ok {
		t.Fatalf("no provider main: %+v", cfg.Providers)
	}
	if p.OAuthProvider != "gpt" {
		t.Fatalf("oauth provider = %q", p.OAuthProvider)
	}
	if p.CustomModelID != "gpt-5.6-sol" || p.OpusModel != "gpt-5.6-sol" || p.SonnetModel != "gpt-5.6-terra" || p.HaikuModel != "gpt-5.6-luna" {
		t.Fatalf("GPT preferred defaults not applied: %+v", p)
	}
}

func TestRunAuthChatGPTLegacyAliasCanonicalizesToGPT(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalLogin := oauthLogin
	oauthLogin = func(_ context.Context, target string, _ oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: target, Backend: "codex", Path: "codex-legacy@example.com.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = originalLogin })

	if err := runAuth(context.Background(), &bytes.Buffer{}, []string{"chatgpt", "legacy"}, authOptions{}); err != nil {
		t.Fatalf("runAuth(chatgpt) error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["legacy"]
	if p.OAuthProvider != "gpt" {
		t.Fatalf("legacy chatgpt login should canonicalize oauthProvider to gpt, got %+v", p)
	}
}

