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
	if p.Env[autoCompactWindowEnv] != "1000000" {
		t.Fatalf("expected %s to be set, got %q", autoCompactWindowEnv, p.Env[autoCompactWindowEnv])
	}
	if p.Env["KEEP_ME"] != "1" {
		t.Fatalf("expected unrelated env to be preserved, got %+v", p.Env)
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
