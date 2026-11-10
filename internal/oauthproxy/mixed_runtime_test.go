package oauthproxy

import (
	"strings"
	"testing"
)

func TestBuildMixedRoutesBucketsByProtocol(t *testing.T) {
	protocols := map[string]string{
		"qwen3.8-max": "anthropic",
		"grok-4.5":    "openai_responses",
		"glm-5.2":     "openai",
	}
	modelSpec := "qwen3.8-max,grok-4.5,glm-5.2"

	routes, err := buildMixedRoutes(modelSpec, protocols)
	if err != nil {
		t.Fatalf("buildMixedRoutes() error: %v", err)
	}
	if len(routes.responses) != 1 || routes.responses[0].Name != "grok-4.5" {
		t.Fatalf("responses routes = %+v", routes.responses)
	}
	if len(routes.chat) != 1 || routes.chat[0].Name != "glm-5.2" {
		t.Fatalf("chat routes = %+v", routes.chat)
	}
	if len(routes.models) != 2 || routes.models[0] != "grok-4.5" || routes.models[1] != "glm-5.2" {
		t.Fatalf("models = %v, want [grok-4.5 glm-5.2]", routes.models)
	}
}

func TestBuildMixedRoutesFallsBackToChat(t *testing.T) {
	protocols := map[string]string{"qwen3.8-max": "anthropic"}
	// glm-5.2 is not in the table: it must fall back to chat, not error, while qwen3.8-max is excluded.
	routes, err := buildMixedRoutes("qwen3.8-max,glm-5.2", protocols)
	if err != nil {
		t.Fatalf("buildMixedRoutes() error: %v", err)
	}
	if len(routes.chat) != 1 || routes.chat[0].Name != "glm-5.2" {
		t.Fatalf("chat routes = %+v, want glm-5.2 fallback", routes.chat)
	}
	if len(routes.models) != 1 || routes.models[0] != "glm-5.2" {
		t.Fatalf("models = %v, want [glm-5.2]", routes.models)
	}
}

func TestBuildMixedRoutesAnthropicOnlyErrors(t *testing.T) {
	protocols := map[string]string{"qwen3.8-max": "anthropic"}
	_, err := buildMixedRoutes("qwen3.8-max", protocols)
	if err == nil {
		t.Fatal("buildMixedRoutes(anthropic-only) = nil error, want error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "anthropic") || !strings.Contains(strings.ToLower(err.Error()), "no longer supports") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildMixedRoutesEmptySpecErrors(t *testing.T) {
	if _, err := buildMixedRoutes("", map[string]string{"a": "openai"}); err == nil {
		t.Fatal("buildMixedRoutes(empty) = nil error, want error")
	}
}

func TestMixedProtocolForModelAliasWins(t *testing.T) {
	protocols := map[string]string{
		"grok-4.5":     "openai_responses",
		"grok-4.5[1m]": "anthropic",
	}
	if got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5[1m]"); got != "anthropic" {
		t.Fatalf("alias protocol = %q, want anthropic (alias wins)", got)
	}
	if got := mixedProtocolForModel(protocols, "glm-5.2", "glm-5.2"); got != "openai" {
		t.Fatalf("unknown model fallback = %q, want openai", got)
	}
	if got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5"); got != "openai_responses" {
		t.Fatalf("name protocol = %q, want openai_responses", got)
	}
}

func TestMixedProtocolForModelFallsBackWhenAliasHasSuffix(t *testing.T) {
	// The alias carries [1m]; the bare name is in the table and must be resolved.
	protocols := map[string]string{"grok-4.5": "openai_responses"}
	got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5[1m]")
	if got != "openai_responses" {
		t.Fatalf("protocol = %q, want openai_responses (name fallback)", got)
	}
}
