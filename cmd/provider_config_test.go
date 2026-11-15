package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestApplyCompactPresetDefaultClearsEveryContextOverride(t *testing.T) {
	p := provider.Provider{
		Model: "gpt-5.5[1m],claude-sonnet-4-6",
		Env: map[string]string{
			maxContextTokensEnv:  "1050000",
			autoCompactWindowEnv: "840000",
			autoCompactPctEnv:    "82",
			"KEEP_ME":            "1",
		},
	}
	applyCompactPreset(&p, compactPresetDefault)

	if p.Model != "gpt-5.5[1m],claude-sonnet-4-6" {
		t.Fatalf("Default changed the model pool markers: %q", p.Model)
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

func TestApplyCompactConfigBalancedWritesExactTriplet(t *testing.T) {
	p := provider.Provider{Env: map[string]string{
		maxContextTokensEnv:  "300000",
		autoCompactWindowEnv: "200000",
		"KEEP_ME":            "1",
	}}
	applyCompactPreset(&p, compactPresetBalanced)

	// The tier is stored as the typed field; the hand-written triplet it
	// replaces is removed, unrelated env is kept, and launch expands the field.
	if p.ContextPreset != provider.ContextPresetNameBalanced500K {
		t.Fatalf("Balanced field = %q", p.ContextPreset)
	}
	if _, ok := p.Env[maxContextTokensEnv]; ok || p.Env["KEEP_ME"] != "1" {
		t.Fatalf("Balanced env = %+v", p.Env)
	}
	want := map[string]string{maxContextTokensEnv: "500000", autoCompactWindowEnv: "500000", autoCompactPctEnv: "85"}
	for key, value := range want {
		if got := provider.ContextPresetEnv(p)[key]; got != value {
			t.Errorf("Balanced %s=%q, want %q", key, got, value)
		}
	}
}

func TestApplyCompactConfigBalanced800KWritesExactTriplet(t *testing.T) {
	p := provider.Provider{Env: map[string]string{
		maxContextTokensEnv:  "500000",
		autoCompactWindowEnv: "500000",
		autoCompactPctEnv:    "85",
		"KEEP_ME":            "1",
	}}
	applyCompactPreset(&p, compactPresetBalanced800K)

	if p.ContextPreset != provider.ContextPresetNameBalanced800K {
		t.Fatalf("Balanced 800K field = %q", p.ContextPreset)
	}
	if _, ok := p.Env[autoCompactPctEnv]; ok || p.Env["KEEP_ME"] != "1" {
		t.Fatalf("Balanced 800K env = %+v", p.Env)
	}
	want := map[string]string{maxContextTokensEnv: "800000", autoCompactWindowEnv: "800000", autoCompactPctEnv: "85"}
	for key, value := range want {
		if got := provider.ContextPresetEnv(p)[key]; got != value {
			t.Errorf("Balanced 800K %s=%q, want %q", key, got, value)
		}
	}
}

func TestCompactPresetOffersSupportedBalancedTiers(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		want         compactPreset
		wantObsolete bool
	}{
		{name: "default", want: compactPresetDefault},
		{name: "balanced 500K", env: map[string]string{
			maxContextTokensEnv: "500000", autoCompactWindowEnv: "500000", autoCompactPctEnv: "85",
		}, want: compactPresetBalanced500K},
		{name: "balanced 800K", env: map[string]string{
			maxContextTokensEnv: "800000", autoCompactWindowEnv: "800000", autoCompactPctEnv: "85",
		}, want: compactPresetBalanced800K},
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
		{name: "extended model", p: provider.Provider{Model: "gpt[1m],other"}, want: "default (200K/1M) · gpt"},
		{name: "balanced 500K", p: provider.Provider{Env: map[string]string{
			maxContextTokensEnv: "500000", autoCompactWindowEnv: "500000", autoCompactPctEnv: "85",
		}}, want: "500K/425K · off"},
		{name: "balanced 800K", p: provider.Provider{Env: map[string]string{
			maxContextTokensEnv: "800000", autoCompactWindowEnv: "800000", autoCompactPctEnv: "85",
		}}, want: "800K/680K · off"},
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

func TestOneMModelsFromProviderDetectsOnlySuffixMarkers(t *testing.T) {
	p := provider.Provider{
		Model: "gpt[1m],model-with-[1m]-inside,haiku [1m] ,subagent[1m]",
	}
	models := oneMModelsFromProvider(p)
	if len(models) != 3 || models[0] != "gpt" || models[1] != "haiku" || models[2] != "subagent" {
		t.Fatalf("detected models = %+v", models)
	}
}
