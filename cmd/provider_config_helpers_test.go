package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestApplyOneMConfigAppliesSuffixAndEnvOnce(t *testing.T) {
	p := provider.Provider{
		OpusModel:     "gpt-5.5",
		SonnetModel:   "sensenova-u1-fast[1m]",
		CustomModelID: "custom-model[1m][1m]",
		SubagentModel: "subagent-model",
		Env: map[string]string{
			"KEEP_ME": "1",
		},
	}

	applyOneMConfig(&p, map[string]bool{
		"opus":     true,
		"sonnet":   true,
		"custom":   true,
		"subagent": true,
	})

	if p.OpusModel != "gpt-5.5[1m]" {
		t.Fatalf("expected opus 1M suffix, got %q", p.OpusModel)
	}
	if p.SonnetModel != "sensenova-u1-fast[1m]" {
		t.Fatalf("expected sonnet suffix to stay single, got %q", p.SonnetModel)
	}
	if p.CustomModelID != "custom-model[1m]" {
		t.Fatalf("expected repeated custom suffixes to collapse, got %q", p.CustomModelID)
	}
	if p.SubagentModel != "subagent-model[1m]" {
		t.Fatalf("expected subagent 1M suffix, got %q", p.SubagentModel)
	}
	// Sizing comes from the per-slot [1m] marker only: ccl declares no context
	// size or compaction threshold of its own.
	for _, key := range []string{maxContextTokensEnv, autoCompactWindowEnv, autoCompactPctEnv} {
		if value, ok := p.Env[key]; ok {
			t.Fatalf("expected %s to stay unset, got %q", key, value)
		}
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("expected unrelated env to be preserved, got %+v", p.Env)
	}
}

func TestCompactPresetRecommendationsAreExact(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", " GPT-5.6-TERRA ", "gpt-5.6-luna[1m]"} {
		if !recommendedOneMModel(model) {
			t.Errorf("expected %q to recommend 1M", model)
		}
	}
	for _, model := range []string{"gpt-5.6", "gpt-5.6-sol-preview", "my-gpt-5.6-terra", "gpt-5.5"} {
		if recommendedOneMModel(model) {
			t.Errorf("did not expect %q to recommend 1M", model)
		}
	}
}

func TestApplyCompactConfigClearsStalePresetsAndKeepsOneMSuffix(t *testing.T) {
	// A provider carrying a preset written by an older ccl version.
	p := provider.Provider{
		OpusModel: "gpt-5.5[1m]",
		Env: map[string]string{
			"KEEP_ME":            "1",
			maxContextTokensEnv:  maxContext300K,
			autoCompactWindowEnv: compactWindow300K,
		},
	}
	applyCompactConfig(&p, map[string]bool{"opus": true}, compactPresetDefault)

	if p.OpusModel != "gpt-5.5[1m]" {
		t.Fatalf("expected the [1m] sizing marker to survive, got %q", p.OpusModel)
	}
	for _, key := range []string{maxContextTokensEnv, autoCompactWindowEnv, autoCompactPctEnv} {
		if value, ok := p.Env[key]; ok {
			t.Fatalf("expected %s cleared so Claude Code sizes the session, got %q", key, value)
		}
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("expected unrelated env preserved, got %+v", p.Env)
	}
}

func TestApplyCompactConfigPreservesCustomEnv(t *testing.T) {
	p := provider.Provider{
		OpusModel: "custom[1m]",
		Env: map[string]string{
			autoCompactWindowEnv: "750000",
			autoCompactPctEnv:    "82",
		},
	}
	applyCompactConfig(&p, map[string]bool{"opus": true}, compactPresetPreserve)

	if p.OpusModel != "custom[1m]" || p.Env[autoCompactWindowEnv] != "750000" || p.Env[autoCompactPctEnv] != "82" {
		t.Fatalf("expected custom config preserved, got model=%q env=%+v", p.OpusModel, p.Env)
	}
}

func TestCompactStateRecognizesLegacyAndCustom(t *testing.T) {
	legacy := compactStateFromProvider(provider.Provider{
		OpusModel: "gpt-5.6-sol[1m]",
		Env:       map[string]string{autoCompactWindowEnv: legacyCompactWindow1M},
	})
	if legacy.preset != compactPresetDefault || legacy.custom {
		t.Fatalf("expected a ccl-written value to map to the default choice, got %+v", legacy)
	}

	custom := compactStateFromProvider(provider.Provider{
		Env: map[string]string{autoCompactWindowEnv: "750000", autoCompactPctEnv: "82"},
	})
	if !custom.custom || custom.preset != compactPresetPreserve {
		t.Fatalf("expected custom preserve state, got %+v", custom)
	}

	// Pairs a previous ccl version wrote are reported as the default choice, so
	// saving again clears them.
	for name, env := range map[string]map[string]string{
		"300K": {maxContextTokensEnv: maxContext300K, autoCompactWindowEnv: compactWindow300K},
		"500K": {autoCompactWindowEnv: legacyCompactWindow500K, autoCompactPctEnv: legacyCompactPct500K},
		"1M":   {maxContextTokensEnv: maxContext1M, autoCompactWindowEnv: compactWindow1M},
	} {
		state := compactStateFromProvider(provider.Provider{OpusModel: "gpt-5.5[1m]", Env: env})
		if state.preset != compactPresetDefault || state.custom {
			t.Fatalf("%s preset should migrate to the default choice, got %+v", name, state)
		}
	}
}

func TestCompactStateSummaries(t *testing.T) {
	tests := []struct {
		name string
		p    provider.Provider
		want string
	}{
		{
			name: "one m",
			p: provider.Provider{OpusModel: "gpt-5.6-sol[1m]", Env: map[string]string{
				maxContextTokensEnv:  maxContext1M,
				autoCompactWindowEnv: compactWindow1M,
			}},
			want: "default · opus",
		},
		{
			name: "legacy",
			p: provider.Provider{OpusModel: "gpt-5.6-sol[1m]", Env: map[string]string{
				autoCompactWindowEnv: legacyCompactWindow1M,
			}},
			want: "default · opus",
		},
		{
			name: "300k with context",
			p: provider.Provider{
				OpusModel: "gpt-5.5[1m]",
				Env: map[string]string{
					maxContextTokensEnv:  maxContext300K,
					autoCompactWindowEnv: compactWindow300K,
				},
			},
			want: "default · opus",
		},
		{
			name: "500k",
			p: provider.Provider{Env: map[string]string{
				maxContextTokensEnv:  maxContext500K,
				autoCompactWindowEnv: compactWindow500K,
			}},
			want: "default · off",
		},
		{
			name: "default",
			p:    provider.Provider{},
			want: "default · off",
		},
		{
			name: "custom",
			p: provider.Provider{Env: map[string]string{
				autoCompactWindowEnv: "750000",
				autoCompactPctEnv:    "82",
			}},
			want: "custom · off",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerOneMSummary(tt.p); got != tt.want {
				t.Fatalf("providerOneMSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyCompactConfigDefaultRemovesOnlyManagedValues(t *testing.T) {
	p := provider.Provider{
		OpusModel: "gpt-5.6-sol[1m]",
		Env: map[string]string{
			maxContextTokensEnv:  maxContext1M,
			autoCompactWindowEnv: compactWindow1M,
			autoCompactPctEnv:    legacyCompactPct1M,
			"KEEP_ME":            "1",
		},
	}
	// Default removes compact env but keeps [1m] when the slot is still selected.
	applyCompactConfig(&p, map[string]bool{"opus": true}, compactPresetDefault)

	if p.OpusModel != "gpt-5.6-sol[1m]" {
		t.Fatalf("expected suffix kept under Claude default, got %q", p.OpusModel)
	}
	if _, ok := p.Env[autoCompactWindowEnv]; ok {
		t.Fatalf("expected compact window removed, got %+v", p.Env)
	}
	if _, ok := p.Env[maxContextTokensEnv]; ok {
		t.Fatalf("expected fallback context removed, got %+v", p.Env)
	}
	if _, ok := p.Env[autoCompactPctEnv]; ok {
		t.Fatalf("expected compact percentage removed, got %+v", p.Env)
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("expected unrelated env preserved, got %+v", p.Env)
	}
}

func TestApplyOneMConfigClearsSuffixAndAutoCompactWhenOff(t *testing.T) {
	p := provider.Provider{
		OpusModel:     "gpt-5.5[1m]",
		SonnetModel:   "sensenova-u1-fast[1m][1m]",
		SubagentModel: "subagent-model[1m]",
		Env: map[string]string{
			autoCompactWindowEnv: "1000000",
			"KEEP_ME":            "1",
		},
	}

	applyOneMConfig(&p, nil)

	if p.OpusModel != "gpt-5.5" {
		t.Fatalf("expected opus suffix to be cleared, got %q", p.OpusModel)
	}
	if p.SonnetModel != "sensenova-u1-fast" {
		t.Fatalf("expected repeated sonnet suffixes to be cleared, got %q", p.SonnetModel)
	}
	if p.SubagentModel != "subagent-model" {
		t.Fatalf("expected subagent suffix to be cleared, got %q", p.SubagentModel)
	}
	if _, ok := p.Env[autoCompactWindowEnv]; ok {
		t.Fatalf("expected %s to be removed, got %+v", autoCompactWindowEnv, p.Env)
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("expected unrelated env to be preserved, got %+v", p.Env)
	}
}

func TestApplyOneMConfigClearsEmptyEnvMap(t *testing.T) {
	p := provider.Provider{
		OpusModel: "gpt-5.5[1m]",
		Env: map[string]string{
			autoCompactWindowEnv: "1000000",
		},
	}

	applyOneMConfig(&p, map[string]bool{})

	if p.OpusModel != "gpt-5.5" {
		t.Fatalf("expected opus suffix to be cleared, got %q", p.OpusModel)
	}
	if p.Env != nil {
		t.Fatalf("expected empty env map to be nil, got %+v", p.Env)
	}
}

func TestOneMSlotsFromProviderDetectsOnlySuffixMarkers(t *testing.T) {
	p := provider.Provider{
		OpusModel:     "gpt-5.5[1m]",
		SonnetModel:   "model-with-[1m]-inside",
		HaikuModel:    "sensenova-lite [1m] ",
		CustomModelID: "custom",
		SubagentModel: "subagent[1m]",
	}

	slots := oneMSlotsFromProvider(p)

	if !slots["opus"] {
		t.Fatalf("expected opus 1M slot to be detected")
	}
	if !slots["haiku"] {
		t.Fatalf("expected haiku 1M slot with surrounding whitespace to be detected")
	}
	if !slots["subagent"] {
		t.Fatalf("expected subagent 1M slot to be detected")
	}
	if slots["sonnet"] {
		t.Fatalf("did not expect non-suffix marker to be treated as 1M")
	}
	if slots["custom"] {
		t.Fatalf("did not expect custom slot to be enabled")
	}
}

func TestModelDisplayName(t *testing.T) {
	if got := modelDisplayName("grok-4.5[1m]"); got != "grok-4.5 (1M)" {
		t.Fatalf("display name = %q", got)
	}
	if got := modelDisplayName("grok-4.5"); got != "grok-4.5" {
		t.Fatalf("plain display name = %q", got)
	}
	if got := modelDisplayName("x[1m][1m]"); got != "x (1M)" {
		t.Fatalf("collapsed display name = %q", got)
	}
}
