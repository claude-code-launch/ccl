package provider_test

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestPreferredOAuthSlotDefaultsGPT(t *testing.T) {
	for _, name := range []string{"gpt", "chatgpt"} {
		custom, opus, sonnet, haiku, ok := provider.PreferredOAuthSlotDefaults(name)
		if !ok {
			t.Fatalf("expected %s defaults", name)
		}
		if custom != "gpt-5.6-sol" || opus != "gpt-5.6-sol" || sonnet != "gpt-5.6-terra" || haiku != "gpt-5.6-luna" {
			t.Fatalf("%s defaults = %q %q %q %q", name, custom, opus, sonnet, haiku)
		}
	}
}

func TestPreferredOAuthSlotDefaultsGrok(t *testing.T) {
	custom, opus, sonnet, haiku, ok := provider.PreferredOAuthSlotDefaults("grok")
	if !ok {
		t.Fatal("expected grok defaults")
	}
	if custom != "grok-4.6" || opus != "grok-4.6" || sonnet != "grok-4.5" || haiku != "grok-4.5" {
		t.Fatalf("grok defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
	if _, _, _, _, ok := provider.PreferredOAuthSlotDefaults("copilot"); ok {
		t.Fatal("copilot should not have preferred defaults")
	}
}

func TestPreferredOAuthSlotDefaultsGemini(t *testing.T) {
	custom, opus, sonnet, haiku, ok := provider.PreferredOAuthSlotDefaults("gemini")
	if !ok {
		t.Fatal("expected gemini defaults")
	}
	if custom != "claude-opus-4-6-thinking" || opus != "claude-opus-4-6-thinking" ||
		sonnet != "claude-sonnet-4-6" || haiku != "gemini-3.1-pro-low" {
		t.Fatalf("gemini defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
}

func TestPreferredOAuthSlotDefaultsKiro(t *testing.T) {
	custom, opus, sonnet, haiku, ok := provider.PreferredOAuthSlotDefaults("kiro")
	if !ok {
		t.Fatal("expected kiro defaults")
	}
	if custom != "claude-opus-4-6" || opus != "claude-opus-4-6" ||
		sonnet != "claude-sonnet-4-6" || haiku != "claude-haiku-4-5" {
		t.Fatalf("kiro defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
}

func TestPreferredOAuthSlotDefaultsKimi(t *testing.T) {
	custom, opus, sonnet, haiku, ok := provider.PreferredOAuthSlotDefaults("kimi")
	if !ok {
		t.Fatal("expected kimi defaults")
	}
	if custom != "kimi-for-coding" || opus != "kimi-for-coding" ||
		sonnet != "kimi-for-coding" || haiku != "kimi-for-coding-highspeed" {
		t.Fatalf("kimi defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
}

// Kimi exposes no model-list endpoint, so the runtime can only start when these
// defaults have filled the empty slots.
func TestApplyOAuthSlotDefaultsMakesKimiRunnable(t *testing.T) {
	p := provider.Provider{OAuthProvider: "kimi"}
	provider.ApplyOAuthSlotDefaults(&p)
	if spec := provider.RuntimeModelSpec(p); spec == "" {
		t.Fatal("kimi still has an empty model spec; the runtime would refuse to start")
	}
}

func TestApplyOAuthSlotDefaultsFillsEmptyOnly(t *testing.T) {
	p := provider.Provider{OAuthProvider: "grok", SonnetModel: "my-custom-sonnet"}
	provider.ApplyOAuthSlotDefaults(&p)
	if p.CustomModelID != "grok-4.6" || p.OpusModel != "grok-4.6" || p.HaikuModel != "grok-4.5" {
		t.Fatalf("empty slots not filled: %+v", p)
	}
	if p.SonnetModel != "my-custom-sonnet" {
		t.Fatalf("existing sonnet was overwritten: %q", p.SonnetModel)
	}
}

func TestApplyOAuthSlotDefaultsGeminiFillsEmptyOnly(t *testing.T) {
	p := provider.Provider{OAuthProvider: "gemini", OpusModel: "my-opus"}
	provider.ApplyOAuthSlotDefaults(&p)
	if p.CustomModelID != "claude-opus-4-6-thinking" || p.SonnetModel != "claude-sonnet-4-6" || p.HaikuModel != "gemini-3.1-pro-low" {
		t.Fatalf("empty gemini slots not filled: %+v", p)
	}
	if p.OpusModel != "my-opus" {
		t.Fatalf("existing opus was overwritten: %q", p.OpusModel)
	}
}

func TestClearUnavailablePreferredDefaults(t *testing.T) {
	p := provider.Provider{
		OAuthProvider: "grok",
		CustomModelID: "grok-4.6",
		OpusModel:     "grok-4.6",
		SonnetModel:   "grok-4.5",
		HaikuModel:    "grok-4.5",
	}
	// Catalog missing sonnet + haiku preferred IDs; keep a user custom sonnet-like
	// value that is not the preferred default.
	available := []string{"grok-4.6", "grok-4", "grok-2-mini"}
	provider.ClearUnavailablePreferredDefaults(&p, available)
	if p.CustomModelID != "grok-4.6" || p.OpusModel != "grok-4.6" {
		t.Fatalf("available preferred defaults were cleared: %+v", p)
	}
	if p.SonnetModel != "" || p.HaikuModel != "" {
		t.Fatalf("missing preferred defaults should clear: %+v", p)
	}

	p.SonnetModel = "my-pinned-sonnet"
	provider.ClearUnavailablePreferredDefaults(&p, available)
	if p.SonnetModel != "my-pinned-sonnet" {
		t.Fatalf("user pin should not be cleared: %q", p.SonnetModel)
	}
}

func TestClearUnavailablePreferredDefaultsMigratesLegacyGrokDefaults(t *testing.T) {
	p := provider.Provider{
		OAuthProvider: "grok",
		CustomModelID: "grok-4.5",
		OpusModel:     "grok-4.5",
		SonnetModel:   "grok-4.3",
		HaikuModel:    "grok-3-mini",
	}
	provider.ClearUnavailablePreferredDefaults(&p, []string{"grok-4.6", "grok-4.5"})
	if p.CustomModelID != "grok-4.6" || p.OpusModel != "grok-4.6" {
		t.Fatalf("legacy top-tier defaults were not migrated: %+v", p)
	}
	if p.SonnetModel != "grok-4.5" || p.HaikuModel != "grok-4.5" {
		t.Fatalf("retired generated defaults were not migrated: %+v", p)
	}
}
