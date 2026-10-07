package claude

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestCustomModelIsNotOverriddenByThePoolFallback pins the audit finding: with
// any slot empty (Fable included) the pool fallback used to write
// ANTHROPIC_MODEL = <sonnet>, which Claude Code ranks above settings.model, so
// the provider launched on Sonnet instead of the chosen custom model.
func TestCustomModelIsNotOverriddenByThePoolFallback(t *testing.T) {
	p := provider.Provider{
		Type: "openai", Endpoint: "https://gw.example/v1",
		Model:         "deepseek-v4-pro-0813,deepseek-v4.1-flash,MiniMax-M2,glm-5.2",
		OpusModel:     "deepseek-v4-pro-0813",
		SonnetModel:   "deepseek-v4-pro-0813",
		HaikuModel:    "glm-5.2",
		CustomModelID: "deepseek-v4.1-flash",
	}
	env := buildEnvWithModelNames(p, "", false, nil)
	if got, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Fatalf("ANTHROPIC_MODEL = %q overrides the custom model", got)
	}
	// An empty Fable slot mirrors Opus rather than whatever the pool scores
	// as "max".
	if got := env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != "deepseek-v4-pro-0813" {
		t.Fatalf("Fable = %q, want it to mirror Opus", got)
	}
	settings := (&providerContext{provider: p}).settings()
	if settings.Model != "deepseek-v4.1-flash" {
		t.Fatalf("settings.model = %q", settings.Model)
	}
}

func TestPoolOnlyProviderFallback(t *testing.T) {
	pool := "glm-5.2,deepseek-v4-pro,MiniMax-M2,qwen3.8-max"

	// No custom model: the pool default still pins the session model.
	env := buildEnvWithModelNames(provider.Provider{Type: "openai", Model: pool}, "", false, nil)
	if env["ANTHROPIC_MODEL"] == "" || env["ANTHROPIC_MODEL"] != env["ANTHROPIC_DEFAULT_SONNET_MODEL"] {
		t.Fatalf("pool-only default: ANTHROPIC_MODEL=%q sonnet=%q", env["ANTHROPIC_MODEL"], env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
	}
	if env["ANTHROPIC_DEFAULT_FABLE_MODEL"] != env["ANTHROPIC_DEFAULT_OPUS_MODEL"] {
		t.Fatalf("pool without a Fable model: fable=%q opus=%q", env["ANTHROPIC_DEFAULT_FABLE_MODEL"], env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}

	// With a custom model the fallback fills the slots but leaves the default
	// model to settings.model.
	env = buildEnvWithModelNames(provider.Provider{Type: "openai", Model: pool, CustomModelID: "glm-5.2"}, "", false, nil)
	if got, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Fatalf("ANTHROPIC_MODEL = %q with a custom model", got)
	}

	// A pool with a real Fable-family model maps it.
	env = buildEnvWithModelNames(provider.Provider{Type: "openai", Model: "glm-5.2,acme-fable-1,acme-opus"}, "", false, nil)
	if got := env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != "acme-fable-1" {
		t.Fatalf("Fable = %q, want the pool's Fable model", got)
	}
}

// TestExplicitAnthropicModelEnvStillWins keeps the escape hatch: a provider
// that sets ANTHROPIC_MODEL in its Env gets it even with a custom model.
func TestExplicitAnthropicModelEnvStillWins(t *testing.T) {
	p := provider.Provider{
		Type: "openai", Model: "a,b", CustomModelID: "a",
		Env: map[string]string{"ANTHROPIC_MODEL": "b"},
	}
	if got := buildEnvWithModelNames(p, "", false, nil)["ANTHROPIC_MODEL"]; got != "b" {
		t.Fatalf("explicit ANTHROPIC_MODEL = %q, want b", got)
	}
}

// TestLegacyBalancedPresetLaunchesWithCurrentValues covers configs saved
// before 2026-09-15, when both Balanced tiers used an 80% threshold: they used
// to be dropped silently at every launch. They now launch as the same tier with
// today's values.
func TestLegacyBalancedPresetLaunchesWithCurrentValues(t *testing.T) {
	for _, tier := range []struct{ window, wantPct string }{
		{provider.Balanced500KMaxContextTokens, provider.Balanced500KAutoCompactPct},
		{provider.Balanced800KMaxContextTokens, provider.Balanced800KAutoCompactPct},
	} {
		env := map[string]string{
			provider.EnvMaxContextTokens:  tier.window,
			provider.EnvAutoCompactWindow: tier.window,
			provider.EnvAutoCompactPct:    provider.LegacyBalancedAutoCompactPct,
		}
		if provider.ContextPresetFromEnv(env) == provider.ContextPresetDefault {
			t.Fatalf("legacy %s triplet is not recognized as Balanced", tier.window)
		}
		if dropped := applyContextPolicy(env); dropped {
			t.Fatalf("legacy %s triplet was dropped", tier.window)
		}
		if env[provider.EnvMaxContextTokens] != tier.window || env[provider.EnvAutoCompactWindow] != tier.window ||
			env[provider.EnvAutoCompactPct] != tier.wantPct {
			t.Fatalf("legacy %s triplet launched as %v", tier.window, env)
		}
	}
}

func TestUnsupportedContextOverrideIsDroppedAndReported(t *testing.T) {
	p := provider.Provider{
		Type: "openai", Model: "a",
		Env: map[string]string{
			provider.EnvMaxContextTokens:  "600000",
			provider.EnvAutoCompactWindow: "600000",
			provider.EnvAutoCompactPct:    "70",
		},
	}
	c := &providerContext{provider: p}
	settings := c.settings()
	if !c.droppedContextOverride {
		t.Fatal("an unsupported context override was not reported")
	}
	for _, key := range provider.ManagedContextEnvKeys() {
		if _, ok := settings.Env[key]; ok {
			t.Fatalf("%s survived an unsupported override", key)
		}
	}

	// The current presets and an empty configuration are not reported.
	for _, env := range []map[string]string{
		nil,
		{
			provider.EnvMaxContextTokens:  provider.Balanced500KMaxContextTokens,
			provider.EnvAutoCompactWindow: provider.Balanced500KAutoCompactWindow,
			provider.EnvAutoCompactPct:    provider.Balanced500KAutoCompactPct,
		},
	} {
		c := &providerContext{provider: provider.Provider{Type: "openai", Model: "a", Env: env}}
		c.settings()
		if c.droppedContextOverride {
			t.Fatalf("supported context %v was reported as dropped", env)
		}
	}
}

func TestModelDisplayNameMarksOneMillionContext(t *testing.T) {
	for model, want := range map[string]string{
		"grok-4.5[1m]": "grok-4.5 (1M)",
		"grok-4.5":     "grok-4.5",
		"x[1m][1m]":    "x (1M)",
	} {
		if got := modelDisplayName(model); got != want {
			t.Fatalf("modelDisplayName(%q) = %q, want %q", model, got, want)
		}
	}
}
