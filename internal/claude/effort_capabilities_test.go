package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func seedClaudeEffortMetadata(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	path, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const data = `{"fixture":{"models":{
		"acme/standard":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]},
		"acme/deep":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh","max","ultra"]}]},
		"acme/plain":{"reasoning":false},
		"acme/budget":{"reasoning":true,"reasoning_options":[{"type":"budget_tokens"}]},
		"acme/toggle":{"reasoning":true,"reasoning_options":[{"type":"toggle"}]},
		"acme/unsupported-tiers":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["minimal","ultra"]}]}
	}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeEffortCapabilitiesFollowActualModel(t *testing.T) {
	seedClaudeEffortMetadata(t)
	c := providerContext{
		provider: provider.Provider{
			Model:             "acme/standard[1m],acme/deep,acme/plain,acme/budget,acme/toggle,acme/unsupported-tiers,unknown-model",
			ModelsDevProvider: "fixture", OpusModel: "acme/standard[1m]", HaikuModel: "acme/plain", EffortLevel: "high",
			ModelOverrides: map[string]string{"claude-opus-4-6": "acme/standard"},
		},
		modelNames: map[string]string{"acme/deep": "Deep Model"},
	}
	settings := c.settingsForCWD(t.TempDir())
	rules := settings.Env[modelCapabilitiesEnv]
	for _, want := range []string{
		"acme/standard=effort,-xhigh_effort,-max_effort",
		"claude-opus-4-6=effort,-xhigh_effort,-max_effort",
		"acme/deep=effort,xhigh_effort,max_effort",
		"deep model=effort,xhigh_effort,max_effort",
		"acme/plain=-effort,-xhigh_effort,-max_effort",
		"acme/budget=-effort,-xhigh_effort,-max_effort",
		"acme/toggle=-effort,-xhigh_effort,-max_effort",
		"acme/unsupported-tiers=-effort,-xhigh_effort,-max_effort",
	} {
		if !strings.Contains(";"+rules+";", ";"+want+";") {
			t.Fatalf("missing %q in %q", want, rules)
		}
	}
	if strings.Contains(rules, "unknown-model=") || strings.Contains(rules, "[1m]") || strings.Contains(rules, "thinking") {
		t.Fatalf("invented unknown capabilities or changed unrelated controls: %q", rules)
	}
	if settings.EffortLevel != "high" || settings.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "" {
		t.Fatal("capability hints changed/pinned the user's starting effort")
	}
}

func TestClaudeEffortCapabilitiesFallbackAndOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := providerContext{
		provider: provider.Provider{Model: "known,plain,unknown"},
		modelDetails: map[string]protocol.ModelInfo{
			"known":   {ReasoningLevels: []protocol.ReasoningLevel{{Effort: "high"}, {Effort: "max"}}},
			"plain":   {ReasoningKnown: true},
			"unknown": {},
		},
	}
	env := map[string]string{}
	c.applyModelEffortCapabilities(env)
	if env[modelCapabilitiesEnv] != "known=effort,-xhigh_effort,max_effort;plain=-effort,-xhigh_effort,-max_effort" {
		t.Fatalf("runtime metadata fallback = %q", env[modelCapabilitiesEnv])
	}
	for _, value := range []string{"", "manual-model=effort,max_effort"} {
		env[modelCapabilitiesEnv] = value
		c.applyModelEffortCapabilities(env)
		if env[modelCapabilitiesEnv] != value {
			t.Fatal("explicit provider capabilities were overwritten")
		}
	}
	c.provider.OAuthProvider = "claude"
	env = map[string]string{}
	c.applyModelEffortCapabilities(env)
	if _, ok := env[modelCapabilitiesEnv]; ok {
		t.Fatal("native subscription model handling was changed")
	}
}

func TestClaudeEffortCapabilitiesDoNotGuessConflictingAliases(t *testing.T) {
	seedClaudeEffortMetadata(t)
	c := providerContext{
		provider:   provider.Provider{ModelsDevProvider: "fixture", Model: "acme/standard,acme/deep,acme/plain"},
		modelNames: map[string]string{"acme/standard": "Shared", "acme/deep": "Shared", "acme/plain": "unsafe;*=effort"},
	}
	env := map[string]string{}
	c.applyModelEffortCapabilities(env)
	rules := env[modelCapabilitiesEnv]
	if strings.Contains(rules, "shared=") || strings.Contains(rules, "unsafe") || strings.Contains(rules, "*") {
		t.Fatalf("ambiguous alias or injected rule escaped: %q", rules)
	}
	if !strings.Contains(rules, "acme/plain=-effort") {
		t.Fatal("unsafe display name lost the valid technical ID's metadata")
	}
}

func TestClaudeEffortCapabilitiesClearPreviousProviderHints(t *testing.T) {
	settings := settingsJSON{Env: map[string]string{}}
	inherited := []string{modelCapabilitiesEnv + "=*=effort,max_effort", "KEEP=kept"}
	for _, value := range buildProcessEnv(inherited, settings, false) {
		if strings.HasPrefix(value, modelCapabilitiesEnv+"=") {
			t.Fatal("previous provider's model capabilities survived into an unknown catalog")
		}
	}
	if value, exists := completeAgentEnv(settings, false)[modelCapabilitiesEnv]; !exists || value != "" {
		t.Fatal("long-lived adapter did not neutralize old provider capabilities")
	}
}
