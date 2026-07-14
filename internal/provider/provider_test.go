package provider_test

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProtocolLabel(t *testing.T) {
	testCases := []struct {
		name         string
		providerType string
		want         string
	}{
		{"anthropic", "anthropic", "anthropic"},
		{"anthropic mixed case", "Anthropic", "anthropic"},
		{"openai chat", "openai", "openai(chat)"},
		{"openai chat display label", "openai(chat)", "openai(chat)"},
		{"openai responses canonical", "openai_responses", "openai(responses)"},
		{"openai responses hyphenated", "openai-responses", "openai(responses)"},
		{"openai responses bare", "responses", "openai(responses)"},
		{"openai responses display label", "openai(responses)", "openai(responses)"},
		{"openai responses legacy display label", "openai(agent)", "openai(responses)"},
		{"empty", "", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.ProtocolLabel(tc.providerType); got != tc.want {
				t.Errorf("ProtocolLabel(%q) = %q, want %q", tc.providerType, got, tc.want)
			}
		})
	}
}

func TestIsOpenAIResponsesType(t *testing.T) {
	for _, v := range []string{"openai_responses", "openai-responses", "responses", "OPENAI_RESPONSES", "openai(responses)", "openai(agent)"} {
		if !provider.IsOpenAIResponsesType(v) {
			t.Errorf("IsOpenAIResponsesType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"openai", "openai(chat)", "anthropic", ""} {
		if provider.IsOpenAIResponsesType(v) {
			t.Errorf("IsOpenAIResponsesType(%q) = true, want false", v)
		}
	}
}

func TestIsOpenAICompatibleType(t *testing.T) {
	for _, v := range []string{"openai", "openai(chat)", "openai_responses", "openai-responses", "responses", "openai(responses)", "openai(agent)"} {
		if !provider.IsOpenAICompatibleType(v) {
			t.Errorf("IsOpenAICompatibleType(%q) = false, want true", v)
		}
	}
	if provider.IsOpenAICompatibleType("anthropic") {
		t.Errorf("IsOpenAICompatibleType(\"anthropic\") = true, want false")
	}
}

func TestIsAnthropicType(t *testing.T) {
	for _, v := range []string{"anthropic", "Anthropic", " ANTHROPIC "} {
		if !provider.IsAnthropicType(v) {
			t.Errorf("IsAnthropicType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"openai", "openai(chat)", "openai(agent)", ""} {
		if provider.IsAnthropicType(v) {
			t.Errorf("IsAnthropicType(%q) = true, want false", v)
		}
	}
}
