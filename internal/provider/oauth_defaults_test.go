package provider_test

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestOAuthDefaultModels(t *testing.T) {
	if got := provider.OAuthDefaultModels("kimi"); got != "kimi-for-coding,kimi-for-coding-highspeed" {
		t.Fatalf("kimi default models = %q", got)
	}
	if got := provider.OAuthDefaultModels("kiro"); got != "claude-opus-4-6,claude-sonnet-4-6,claude-haiku-4-5" {
		t.Fatalf("kiro default models = %q", got)
	}
	for _, live := range []string{"gpt", "grok", "gemini", "qoder", "workbuddy", "workbuddycn", "cursor", "autoclaw", ""} {
		if got := provider.OAuthDefaultModels(live); got != "" {
			t.Fatalf("%s should rely on live discovery, got %q", live, got)
		}
	}
}

// Kimi exposes no model-list endpoint, so RuntimeModelSpec falls back to the
// built-in Kimi pool when Model is empty.
func TestRuntimeModelSpecMakesKimiAndKiroRunnable(t *testing.T) {
	for _, backend := range []string{"kimi", "kiro"} {
		p := provider.Provider{OAuthProvider: backend}
		if spec := provider.RuntimeModelSpec(p); spec == "" {
			t.Fatalf("%s has an empty runtime model spec", backend)
		}
	}
}

func TestNormalizeProviderRewritesRetiredGeminiModel(t *testing.T) {
	p := provider.Provider{
		OAuthProvider: "gemini",
		Type:          "openai",
		Model:         "gemini-3.1-pro-low,claude-sonnet-4-6",
	}
	if !provider.NormalizeProvider(&p) {
		t.Fatal("expected the retired Gemini id to change")
	}
	if p.Model != "gemini-3.8-flash,claude-sonnet-4-6" {
		t.Fatalf("migrated = %+v", p)
	}
	if provider.NormalizeProvider(&p) {
		t.Fatal("second pass should leave the provider unchanged")
	}
}

func TestNormalizeProviderPreservesExplicitGemini3Flash(t *testing.T) {
	p := provider.Provider{
		OAuthProvider: "gemini",
		Type:          "openai",
		Model:         "gemini-3-flash,claude-sonnet-4-6",
	}
	provider.NormalizeProvider(&p)
	if p.Model != "gemini-3-flash,claude-sonnet-4-6" {
		t.Fatalf("migrated = %+v", p)
	}
	if provider.NormalizeProvider(&p) {
		t.Fatal("second pass should leave the provider unchanged")
	}
}

func TestNormalizeRetiredGeminiPreservesContextMarkers(t *testing.T) {
	p := provider.Provider{
		OAuthProvider: "gemini",
		Model:         "gemini-3.1-pro-low[1m],claude-sonnet-4-6[1m],gemini-3.1-pro-low",
	}
	provider.NormalizeProvider(&p)
	if p.Model != "gemini-3.8-flash[1m],claude-sonnet-4-6[1m],gemini-3.8-flash" {
		t.Fatalf("pool context markers changed: %q", p.Model)
	}
}
