package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProviderTargetSelectsTheRequestedProvider(t *testing.T) {
	cfg := &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a"}, "b": {Name: "b"}, "c": {Name: "c"},
	}}
	for name, testCase := range map[string]struct {
		target providerTarget
		want   string
	}{
		"default is the active provider": {providerTarget{}, "a"},
		"--provider names one":           {providerTarget{name: "c"}, "c"},
	} {
		got, err := testCase.target.resolve(cfg)
		if err != nil || got != testCase.want {
			t.Fatalf("%s: resolve() = %q, %v", name, got, err)
		}
	}
	for name, target := range map[string]providerTarget{
		"unknown provider": {name: "zzz"},
	} {
		if _, err := target.resolve(cfg); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := (providerTarget{}).resolve(&provider.Config{Providers: cfg.Providers}); err == nil {
		t.Fatal("no selection resolved to a provider")
	}
}

// TestEnvSetTargetsAndRefusesManagedKeys covers S10 (any provider, not only
// the active one) and R5 (ccl-managed settings are not raw env variables).
func TestEnvSetTargetsAndRefusesManagedKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai"}, "b": {Name: "b", Type: "openai"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := runEnvSet([]string{"FOO", "1"}, providerTarget{name: "b"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.Providers["b"].Env["FOO"] != "1" || cfg.Providers["a"].Env["FOO"] != "" {
		t.Fatalf("--provider b wrote %+v / %+v", cfg.Providers["a"].Env, cfg.Providers["b"].Env)
	}

	for _, key := range []string{
		"GROK_DEFAULT_MODEL", "grok_config", "GROK_MODELS_LIST_URL",
		provider.EnvMaxContextTokens, provider.EnvAutoCompactPct, "GROK_MODELS_BASE_URL", "XAI_API_KEY",
	} {
		err := runEnvSet([]string{key, "x"}, providerTarget{})
		if err == nil || !strings.Contains(err.Error(), "ccl") {
			t.Fatalf("managed key %s: %v", key, err)
		}
	}
	if err := runEnvMove("FOO", "GROK_SESSION_SUMMARY_MODEL", true, providerTarget{name: "b"}); err == nil {
		t.Fatal("renaming onto a managed key was accepted")
	}
}
