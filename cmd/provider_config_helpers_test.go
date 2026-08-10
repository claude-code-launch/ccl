package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestApplyCompactConfigNormalizesSuffixesAndUsesDefaultContext(t *testing.T) {
	p := provider.Provider{
		OpusModel:     "gpt-5.5",
		SonnetModel:   "sensenova-u1-fast[1m]",
		CustomModelID: "custom-model[1m][1m]",
		SubagentModel: "subagent-model",
		Env:           map[string]string{"KEEP_ME": "1"},
	}

	applyCompactConfig(&p, map[string]bool{
		"opus": true, "sonnet": true, "custom": true, "subagent": true,
	}, compactPresetDefault)

	if p.OpusModel != "gpt-5.5[1m]" || p.SonnetModel != "sensenova-u1-fast[1m]" ||
		p.CustomModelID != "custom-model[1m]" || p.SubagentModel != "subagent-model[1m]" {
		t.Fatalf("[1m] suffixes were not normalized: %+v", p)
	}
	for _, key := range provider.ManagedContextEnvKeys() {
		if value, ok := p.Env[key]; ok {
			t.Fatalf("Default left %s=%q", key, value)
		}
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("unrelated env was removed: %+v", p.Env)
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

func TestApplyCompactConfigDefaultClearsEveryContextOverride(t *testing.T) {
	p := provider.Provider{
		OpusModel: "gpt-5.5[1m]",
		Env: map[string]string{
			maxContextTokensEnv:           "1050000",
			autoCompactWindowEnv:          "840000",
			autoCompactPctEnv:             "82",
			provider.EnvContextBudgetMode: "manual",
			"KEEP_ME":                     "1",
		},
	}
	applyCompactConfig(&p, map[string]bool{"opus": true}, compactPresetDefault)

	if p.OpusModel != "gpt-5.5[1m]" {
		t.Fatalf("Default removed the per-slot marker: %q", p.OpusModel)
	}
	for _, key := range append(provider.ManagedContextEnvKeys(), provider.EnvContextBudgetMode) {
		if value, ok := p.Env[key]; ok {
			t.Fatalf("Default left %s=%q", key, value)
		}
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("unrelated env was removed: %+v", p.Env)
	}
}

func TestApplyCompactConfigBalancedWritesExactTriplet(t *testing.T) {
	p := provider.Provider{Env: map[string]string{
		maxContextTokensEnv:  "300000",
		autoCompactWindowEnv: "200000",
		"KEEP_ME":            "1",
	}}
	applyCompactConfig(&p, nil, compactPresetBalanced)

	want := map[string]string{
		maxContextTokensEnv:  "500000",
		autoCompactWindowEnv: "500000",
		autoCompactPctEnv:    "80",
	}
	for key, value := range want {
		if p.Env[key] != value {
			t.Errorf("Balanced %s=%q, want %q", key, p.Env[key], value)
		}
	}
	if p.Env["KEEP_ME"] != "1" || !provider.IsBalancedContextPreset(p.Env) {
		t.Fatalf("Balanced env = %+v", p.Env)
	}
}

func TestCompactPresetOffersOnlyDefaultAndBalanced(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		want         compactPreset
		wantObsolete bool
	}{
		{name: "default", want: compactPresetDefault},
		{name: "balanced", env: map[string]string{
			maxContextTokensEnv: "500000", autoCompactWindowEnv: "500000", autoCompactPctEnv: "80",
		}, want: compactPresetBalanced},
		{name: "old 300K", env: map[string]string{
			maxContextTokensEnv: "300000", autoCompactWindowEnv: "200000",
		}, want: compactPresetDefault, wantObsolete: true},
		{name: "old 1M", env: map[string]string{
			maxContextTokensEnv: "1000000", autoCompactWindowEnv: "900000",
		}, want: compactPresetDefault, wantObsolete: true},
		{name: "custom", env: map[string]string{
			autoCompactWindowEnv: "750000", autoCompactPctEnv: "82",
		}, want: compactPresetDefault, wantObsolete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := provider.Provider{Env: tt.env}
			if got := compactPresetFromProvider(p); got != tt.want {
				t.Fatalf("preset = %v, want %v", got, tt.want)
			}
			if got := hasUnsupportedContextConfig(p); got != tt.wantObsolete {
				t.Fatalf("unsupported = %t, want %t", got, tt.wantObsolete)
			}
		})
	}
}

func TestCompactStateSummaries(t *testing.T) {
	tests := []struct {
		name string
		p    provider.Provider
		want string
	}{
		{name: "default", p: provider.Provider{}, want: "default (200K/1M) · off"},
		{name: "extended slot", p: provider.Provider{OpusModel: "gpt[1m]"}, want: "default (200K/1M) · opus"},
		{name: "balanced", p: provider.Provider{Env: map[string]string{
			maxContextTokensEnv: "500000", autoCompactWindowEnv: "500000", autoCompactPctEnv: "80",
		}}, want: "500K/400K · off"},
		{name: "obsolete becomes default", p: provider.Provider{Env: map[string]string{
			maxContextTokensEnv: "300000",
		}}, want: "default (200K/1M) · off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerOneMSummary(tt.p); got != tt.want {
				t.Fatalf("providerOneMSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyCompactConfigClearsSuffixAndContextWhenOff(t *testing.T) {
	p := provider.Provider{
		OpusModel: "gpt-5.5[1m]",
		Env:       map[string]string{autoCompactWindowEnv: "1000000"},
	}
	applyCompactConfig(&p, nil, compactPresetDefault)
	if p.OpusModel != "gpt-5.5" || p.Env != nil {
		t.Fatalf("off state = model %q env %+v", p.OpusModel, p.Env)
	}
}

func TestOneMSlotsFromProviderDetectsOnlySuffixMarkers(t *testing.T) {
	p := provider.Provider{
		OpusModel: "gpt[1m]", SonnetModel: "model-with-[1m]-inside",
		HaikuModel: "haiku [1m] ", SubagentModel: "subagent[1m]",
	}
	slots := oneMSlotsFromProvider(p)
	if !slots["opus"] || !slots["haiku"] || !slots["subagent"] || slots["sonnet"] || slots["custom"] {
		t.Fatalf("detected slots = %+v", slots)
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
