package provider_test

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestPreferredOAuthSlotDefaultsGPT(t *testing.T) {
	// Load folds the retired chatgpt/codex names into gpt, so only gpt has
	// defaults here.
	if _, _, _, _, _, ok := provider.PreferredOAuthSlotDefaults("chatgpt"); ok {
		t.Fatal("chatgpt still has its own defaults")
	}
	for _, name := range []string{"gpt"} {
		custom, opus, sonnet, haiku, fable, ok := provider.PreferredOAuthSlotDefaults(name)
		if !ok {
			t.Fatalf("expected %s defaults", name)
		}
		if custom != "gpt-5.6-sol" || opus != "gpt-5.6-sol" || sonnet != "gpt-5.6-terra" || haiku != "gpt-5.6-luna" {
			t.Fatalf("%s defaults = %q %q %q %q", name, custom, opus, sonnet, haiku)
		}
		if fable != opus {
			t.Fatalf("%s fable = %q, want the opus default %q", name, fable, opus)
		}
	}
}

func TestPreferredOAuthSlotDefaultsGrok(t *testing.T) {
	custom, opus, sonnet, haiku, fable, ok := provider.PreferredOAuthSlotDefaults("grok")
	if !ok {
		t.Fatal("expected grok defaults")
	}
	if custom != "grok-4.6" || opus != "grok-4.6" || sonnet != "grok-4.5" || haiku != "grok-4.5" {
		t.Fatalf("grok defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
	if fable != opus {
		t.Fatalf("grok fable = %q, want the opus default %q", fable, opus)
	}
	if _, _, _, _, _, ok := provider.PreferredOAuthSlotDefaults("copilot"); ok {
		t.Fatal("copilot should not have preferred defaults")
	}
}

func TestPreferredOAuthSlotDefaultsGemini(t *testing.T) {
	custom, opus, sonnet, haiku, fable, ok := provider.PreferredOAuthSlotDefaults("gemini")
	if !ok {
		t.Fatal("expected gemini defaults")
	}
	if custom != "claude-opus-4-6-thinking" || opus != "claude-opus-4-6-thinking" ||
		sonnet != "claude-sonnet-4-6" || haiku != "gemini-3.1-pro-low" {
		t.Fatalf("gemini defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
	if fable != opus {
		t.Fatalf("gemini fable = %q, want the opus default %q", fable, opus)
	}
}

func TestPreferredOAuthSlotDefaultsKiro(t *testing.T) {
	custom, opus, sonnet, haiku, fable, ok := provider.PreferredOAuthSlotDefaults("kiro")
	if !ok {
		t.Fatal("expected kiro defaults")
	}
	if custom != "claude-opus-4-6" || opus != "claude-opus-4-6" ||
		sonnet != "claude-sonnet-4-6" || haiku != "claude-haiku-4-5" {
		t.Fatalf("kiro defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
	if fable != opus {
		t.Fatalf("kiro fable = %q, want the opus default %q", fable, opus)
	}
}

func TestPreferredOAuthSlotDefaultsKimi(t *testing.T) {
	custom, opus, sonnet, haiku, fable, ok := provider.PreferredOAuthSlotDefaults("kimi")
	if !ok {
		t.Fatal("expected kimi defaults")
	}
	if custom != "kimi-for-coding" || opus != "kimi-for-coding" ||
		sonnet != "kimi-for-coding" || haiku != "kimi-for-coding-highspeed" {
		t.Fatalf("kimi defaults = %q %q %q %q", custom, opus, sonnet, haiku)
	}
	if fable != opus {
		t.Fatalf("kimi fable = %q, want the opus default %q", fable, opus)
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

// TestLaunchNeverMovesAUserGrokChoice pins S13: launch-time reconciliation
// no longer treats a value an older ccl used as a default as "generated".
// Moving those is a one-time config migration (NormalizeProvider), so a user
// who deliberately picks grok-4.5 for Opus keeps it on every launch.
func TestLaunchNeverMovesAUserGrokChoice(t *testing.T) {
	p := provider.Provider{OAuthProvider: "grok", OpusModel: "grok-4.5", HaikuModel: "grok-3-mini"}
	provider.ClearUnavailablePreferredDefaults(&p, []string{"grok-4.6", "grok-4.5", "grok-3-mini"})
	if p.OpusModel != "grok-4.5" || p.HaikuModel != "grok-3-mini" {
		t.Fatalf("launch moved user choices: %+v", p)
	}
}

func TestNormalizeProviderMigratesLegacyGrokDefaultsOnlyWhenAsked(t *testing.T) {
	legacy := func() provider.Provider {
		return provider.Provider{OAuthProvider: "grok", Type: "openai_responses",
			CustomModelID: "grok-4.5", OpusModel: "grok-4.5", SonnetModel: "grok-4.3", HaikuModel: "grok-3-mini"}
	}
	p := legacy()
	provider.NormalizeProvider(&p, "grok", false)
	if p.OpusModel != "grok-4.5" {
		t.Fatal("slots migrated without legacySlots")
	}
	p = legacy()
	if !provider.NormalizeProvider(&p, "grok", true) {
		t.Fatal("legacy migration reported no change")
	}
	if p.CustomModelID != "grok-4.6" || p.OpusModel != "grok-4.6" || p.SonnetModel != "grok-4.5" || p.HaikuModel != "grok-4.5" {
		t.Fatalf("legacy defaults = %+v", p)
	}
}
