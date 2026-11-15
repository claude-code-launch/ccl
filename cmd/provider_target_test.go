package cmd

import (
	"testing"

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
