package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestAutoDetectDoesNotDowngradeResponsesToChat guards a regression where
// reopening an existing openai_responses (API-key) provider let the auto-probe
// overwrite its stored Type with the probe's coarse "openai" result. The probe
// cannot tell Chat Completions from Responses (both share GET /v1/models), so
// an OpenAI-family result must preserve an existing Responses choice instead of
// downgrading it to chat.
func TestAutoDetectDoesNotDowngradeResponsesToChat(t *testing.T) {
	p := provider.Provider{
		Name:     "resp",
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		APIKey:   "sk-test",
		Model:    "gpt-x",
	}
	m := NewAdvancedConfigModel(&p)
	m.applyModelDetectionResult("openai", "gpt-x,gpt-y", "", "https://example.test/v1", nil)
	if m.p.Type != "openai_responses" {
		t.Fatalf("type downgraded to %q after OpenAI-family probe; want openai_responses", m.p.Type)
	}
}

// TestAutoDetectStillOverwritesOnFamilyChange ensures the guard only preserves
// the Responses choice inside the OpenAI family: a probe landing on a genuinely
// different family (Anthropic) must still overwrite the stored Type.
func TestAutoDetectStillOverwritesOnFamilyChange(t *testing.T) {
	p := provider.Provider{
		Name:     "resp",
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		APIKey:   "sk-test",
		Model:    "gpt-x",
	}
	m := NewAdvancedConfigModel(&p)
	m.applyModelDetectionResult("anthropic", "claude-x,claude-y", "x-api-key", "https://example.test/v1", nil)
	if m.p.Type != "anthropic" {
		t.Fatalf("anthropic probe should overwrite type, got %q", m.p.Type)
	}
}
