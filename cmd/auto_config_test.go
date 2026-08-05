package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func mustFind(t *testing.T, rec AutoRecommendation, slot string) string {
	t.Helper()
	switch slot {
	case "opus":
		return rec.Opus
	case "sonnet":
		return rec.Sonnet
	case "haiku":
		return rec.Haiku
	case "custom":
		return rec.Custom
	case "subagent":
		return rec.Subagent
	}
	t.Fatalf("unknown slot %q", slot)
	return ""
}

func TestRecommendDistinguishesProAndFlashByName(t *testing.T) {
	pool := []string{"gpt-5.6-pro-max", "gpt-5.6-flash", "gpt-5.6-pro"}
	rec := RecommendModels(provider.Provider{}, pool, nil)
	if rec.Opus != "gpt-5.6-pro-max" {
		t.Fatalf("opus = %q, want gpt-5.6-pro-max", rec.Opus)
	}
	if rec.Sonnet != "gpt-5.6-pro" {
		t.Fatalf("sonnet = %q, want gpt-5.6-pro", rec.Sonnet)
	}
	if rec.Haiku != "gpt-5.6-flash" {
		t.Fatalf("haiku = %q, want gpt-5.6-flash", rec.Haiku)
	}
	if rec.Custom != rec.Sonnet {
		t.Fatalf("custom = %q, want sonnet %q", rec.Custom, rec.Sonnet)
	}
	if rec.Subagent != "" {
		t.Fatalf("subagent should stay Auto when no lighter model exists, got %q", rec.Subagent)
	}
}

func TestRecommendOrderIndependent(t *testing.T) {
	poolA := []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat"}
	poolB := []string{"deepseek-v4-flash", "deepseek-chat", "deepseek-v4-pro"}
	a := RecommendModels(provider.Provider{}, poolA, nil)
	b := RecommendModels(provider.Provider{}, poolB, nil)
	for _, slot := range []string{"opus", "sonnet", "haiku", "custom", "subagent"} {
		va, vb := mustFind(t, a, slot), mustFind(t, b, slot)
		if va != vb {
			t.Fatalf("slot %s differs by input order: %q vs %q", slot, va, vb)
		}
	}
}

func TestRecommendFallsBackWithFewModels(t *testing.T) {
	// One model: every slot uses it.
	rec := RecommendModels(provider.Provider{}, []string{"only-model"}, nil)
	for _, slot := range []string{"opus", "sonnet", "haiku", "custom"} {
		if mustFind(t, rec, slot) != "only-model" {
			t.Fatalf("slot %s = %q, want only-model", slot, mustFind(t, rec, slot))
		}
	}

	// Two models: strong -> Opus/Sonnet/Custom, light -> Haiku.
	two := RecommendModels(provider.Provider{}, []string{"qwen-coder-max", "qwen-coder-lite"}, nil)
	if two.Opus != "qwen-coder-max" || two.Sonnet != "qwen-coder-max" || two.Haiku != "qwen-coder-lite" {
		t.Fatalf("two-model fallback wrong: opus=%q sonnet=%q haiku=%q", two.Opus, two.Sonnet, two.Haiku)
	}
	if two.Custom != two.Opus {
		t.Fatalf("custom=%q, want opus %q", two.Custom, two.Opus)
	}

	// Three models: strong/main + light + one spare. Opus/Sonnet/Custom share the
	// strongest model, Haiku takes the lightest, and the spare goes to Subagent.
	three := RecommendModels(provider.Provider{}, []string{"grok-4.5", "grok-4.3", "grok-3-mini"}, nil)
	if three.Opus != "grok-4.5" || three.Sonnet != "grok-4.5" || three.Haiku != "grok-3-mini" {
		t.Fatalf("three-model wrong: %+v", three)
	}
	if three.Custom != three.Opus {
		t.Fatalf("custom=%q, want opus %q", three.Custom, three.Opus)
	}
	if three.Subagent != "grok-4.3" {
		t.Fatalf("subagent should take the spare model, got %q", three.Subagent)
	}
}

func TestRecommendPreservesStillValidSlots(t *testing.T) {
	current := provider.Provider{
		OpusModel:   "deepseek-v4-pro",
		SonnetModel: "deepseek-v4-flash",
		HaikuModel:  "grok-3-mini", // not in the new pool
		// Custom deliberately left unset: it should follow the Sonnet pick.
	}
	rec := RecommendModels(current, []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat"}, nil)
	if rec.Opus != "deepseek-v4-pro" {
		t.Fatalf("opus should be preserved, got %q", rec.Opus)
	}
	if rec.Sonnet != "deepseek-v4-flash" {
		t.Fatalf("sonnet should be preserved, got %q", rec.Sonnet)
	}
	if rec.Custom != rec.Sonnet {
		t.Fatalf("custom should follow sonnet, got %q", rec.Custom)
	}
	// Haiku's old model is gone; it must be re-recommended from the pool.
	if rec.Haiku == "grok-3-mini" || rec.Haiku == "" {
		t.Fatalf("haiku should be re-recommended, got %q", rec.Haiku)
	}
}

func TestRecommendSubagentUsesLighterModelWhenPresent(t *testing.T) {
	pool := []string{"kimi-k2", "kimi-k2-thinking", "kimi-lite", "kimi-flash-mini"}
	rec := RecommendModels(provider.Provider{}, pool, nil)
	if rec.Subagent != "kimi-lite" {
		t.Fatalf("subagent = %q, want kimi-lite", rec.Subagent)
	}
}

func TestRecommendExcludesNonChatModels(t *testing.T) {
	pool := []string{"embedding-v3", "dall-e-3", "whisper-1", "deepseek-chat", "deepseek-v4-flash"}
	rec := RecommendModels(provider.Provider{}, pool, nil)
	if rec.Opus != "deepseek-chat" && rec.Opus != "deepseek-v4-flash" {
		t.Fatalf("opus picked a non-chat model: %q", rec.Opus)
	}
	for _, slot := range []string{"opus", "sonnet", "haiku", "custom", "subagent"} {
		if v := mustFind(t, rec, slot); v == "embedding-v3" || v == "dall-e-3" || v == "whisper-1" {
			t.Fatalf("non-chat model %q leaked into slot %s", v, slot)
		}
	}
}

func TestRecommendOneMAutomaticallyForAllowlist(t *testing.T) {
	rec := RecommendModels(provider.Provider{}, []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}, nil)
	if !rec.OneMSlots["opus"] || !rec.OneMSlots["sonnet"] || !rec.OneMSlots["haiku"] {
		t.Fatalf("allowlist models should auto-enable 1M: %+v", rec.OneMSlots)
	}
}

func TestRecommendOneMNotAutoForReportedOnly(t *testing.T) {
	meta := map[string]protocol.ModelInfo{
		"deepseek-v4-pro":   {ID: "deepseek-v4-pro", ContextWindow: 1000000},
		"deepseek-v4-flash": {ID: "deepseek-v4-flash", ContextWindow: 900000},
	}
	rec := RecommendModels(provider.Provider{}, []string{"deepseek-v4-pro", "deepseek-v4-flash"}, meta)
	if rec.OneMSlots["opus"] || rec.OneMSlots["sonnet"] || rec.OneMSlots["haiku"] {
		t.Fatalf("reported 1M window must not auto-enable the marker: %+v", rec.OneMSlots)
	}
}

func TestRecommendPreservesExistingOneMMarker(t *testing.T) {
	// gpt-5.6-terra is NOT in the allowlist, so a user marker on it must survive.
	current := provider.Provider{SonnetModel: "gpt-5.6-terra[1m]"}
	rec := RecommendModels(current, []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}, nil)
	if !rec.OneMSlots["sonnet"] {
		t.Fatalf("existing [1m] on a preserved non-allowlist model should be kept: %+v", rec.OneMSlots)
	}
	// gpt-5.6-sol / luna are allowlist: auto-enable even without a prior marker.
	if !rec.OneMSlots["opus"] || !rec.OneMSlots["haiku"] {
		t.Fatalf("allowlist models should auto-enable 1M: %+v", rec.OneMSlots)
	}
}

func TestRecommendMetadataBreaksTies(t *testing.T) {
	// Both models score as pro-class by name; the one with the larger window wins.
	meta := map[string]protocol.ModelInfo{
		"gpt-pro":   {ID: "gpt-pro", ContextWindow: 1000000},
		"gpt-pro-b": {ID: "gpt-pro-b", ContextWindow: 128000},
	}
	rec := RecommendModels(provider.Provider{}, []string{"gpt-pro", "gpt-pro-b"}, meta)
	if rec.Opus != "gpt-pro" {
		t.Fatalf("opus should prefer the larger-window tie, got %q", rec.Opus)
	}
}
