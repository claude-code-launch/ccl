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

func TestInferOAuthProvider(t *testing.T) {
	tests := []struct {
		name         string
		providerName string
		endpoint     string
		want         string
	}{
		{name: "ChatGPT Codex backend", providerName: "chatgpt", endpoint: "oauth://codex", want: "chatgpt"},
		{name: "renamed ChatGPT provider", providerName: "my-account", endpoint: "oauth://codex", want: "chatgpt"},
		{name: "legacy Codex provider", providerName: "codex", endpoint: "oauth://codex", want: "codex"},
		{name: "Gemini Antigravity backend", providerName: "gemini", endpoint: "oauth://antigravity", want: "gemini"},
		{name: "Gemini public backend", providerName: "google-account", endpoint: "oauth://gemini", want: "gemini"},
		{name: "Grok xAI backend", providerName: "grok", endpoint: "oauth://xai", want: "grok"},
		{name: "Grok renamed provider", providerName: "my-account", endpoint: "oauth://xai", want: "grok"},
		{name: "Copilot shares codex backend", providerName: "copilot", endpoint: "oauth://codex", want: "chatgpt"},
		{name: "Kimi backend", providerName: "kimi", endpoint: "oauth://kimi", want: "kimi"},
		{name: "Claude backend", providerName: "claude", endpoint: "oauth://claude", want: "claude"},
		{name: "ordinary HTTP provider", providerName: "chatgpt", endpoint: "https://example.test/v1", want: ""},
		{name: "unknown OAuth backend", providerName: "other", endpoint: "oauth://other", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := provider.InferOAuthProvider(tt.providerName, tt.endpoint); got != tt.want {
				t.Fatalf("InferOAuthProvider(%q, %q) = %q, want %q", tt.providerName, tt.endpoint, got, tt.want)
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

func TestRuntimeModelSpecIncludesSlotsSubagentAndOverrides(t *testing.T) {
	p := provider.Provider{
		Model:         "gpt-5.4-mini,gpt-5.6-sol",
		CustomModelID: "gpt-5.4-mini",
		OpusModel:     "gpt-5.6-sol[1m]",
		SonnetModel:   "gpt-5.6-terra",
		HaikuModel:    "gpt-5.6-luna",
		SubagentModel: "gpt-5.6-terra[1m]",
		ModelOverrides: map[string]string{
			"claude-haiku": "gpt-5.4-mini",
			"claude-opus":  "gpt-5.5",
		},
	}
	want := "gpt-5.4-mini,gpt-5.6-sol,gpt-5.6-sol[1m],gpt-5.6-terra,gpt-5.6-luna,gpt-5.6-terra[1m],gpt-5.5"
	if got := provider.RuntimeModelSpec(p); got != want {
		t.Fatalf("RuntimeModelSpec() = %q, want %q", got, want)
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

func TestFixedOAuthProtocol(t *testing.T) {
	got, ok := provider.FixedOAuthProtocol("chatgpt")
	if !ok || got != "openai_responses" {
		t.Fatalf("chatgpt = %q %v", got, ok)
	}
	got, ok = provider.FixedOAuthProtocol("copilot")
	if !ok || got != "openai_responses" {
		t.Fatalf("copilot = %q %v", got, ok)
	}
	got, ok = provider.FixedOAuthProtocol("gemini")
	if !ok || got != "openai" {
		t.Fatalf("gemini = %q %v", got, ok)
	}
	got, ok = provider.FixedOAuthProtocol("grok")
	if !ok || got != "openai" {
		t.Fatalf("grok = %q %v", got, ok)
	}
	got, ok = provider.FixedOAuthProtocol("kimi")
	if !ok || got != "openai" {
		t.Fatalf("kimi = %q %v", got, ok)
	}
	got, ok = provider.FixedOAuthProtocol("claude")
	if !ok || got != "anthropic" {
		t.Fatalf("claude = %q %v", got, ok)
	}
	if _, ok := provider.FixedOAuthProtocol(""); ok {
		t.Fatal("empty should not fix")
	}
}
