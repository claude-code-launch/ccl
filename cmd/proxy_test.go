package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProxyCommandDoesNotFallThroughToClaude(t *testing.T) {
	if !isCclCommand("proxy") {
		t.Fatal("proxy would launch Claude Code")
	}
	cmd := newProxyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--provider", "start", "stop", "status"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("proxy help missing %s", text)
		}
	}
}

func TestProxyRejectsAmbiguousProviderSelection(t *testing.T) {
	cmd := newProxyCommand()
	cmd.SetArgs([]string{"start", "provider1", "-p", "provider2"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("error=%v", err)
	}
}

// default is Claude Code's own subscription: starting the proxy for it, by
// name or as the active selection, starts nothing and needs no provider.
func TestProxyStartOnDefaultStartsNothing(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: provider.DefaultProvider, Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})
	// A regression would start a real proxy: keep its settings overlay away
	// from the developer's own Claude configuration.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
	for _, args := range [][]string{{}, {"start"}, {"start", " "}, {"start", "default"}} {
		cmd := newProxyCommand()
		cmd.SetArgs(args)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "needs no proxy") {
			t.Fatalf("args %q: out=%q err=%v", args, out.String(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("Claude settings were touched: %v", err)
	}
	cmd := newProxyCommand()
	cmd.SetArgs([]string{"start", "missing"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `provider "missing" not found`) {
		t.Fatalf("explicit provider: error = %v", err)
	}
}
