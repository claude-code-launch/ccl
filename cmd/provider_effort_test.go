package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProviderEffortAndUltracode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai"}, "b": {Name: "b", Type: "openai"},
	}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runProviderEffort(&out, []string{"xhigh"}, providerTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := runProviderUltracode(&out, "on", providerTarget{name: "b"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.Providers["a"].EffortLevel != "xhigh" || cfg.Providers["b"].EffortLevel != "" {
		t.Fatalf("effort = %q / %q", cfg.Providers["a"].EffortLevel, cfg.Providers["b"].EffortLevel)
	}
	if !cfg.Providers["b"].Ultracode || cfg.Providers["a"].Ultracode {
		t.Fatal("ultracode went to the wrong provider")
	}

	if err := runProviderEffort(&out, []string{"default"}, providerTarget{}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.Load(); cfg.Providers["a"].EffortLevel != "" {
		t.Fatal("default did not clear the effort")
	}
	if err := runProviderEffort(&out, []string{"max!"}, providerTarget{}); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	out.Reset()
	if err := runProviderEffort(&out, nil, providerTarget{name: "b"}); err != nil || !strings.Contains(out.String(), "default + ultracode") {
		t.Fatalf("show = %q, %v", out.String(), err)
	}
}
