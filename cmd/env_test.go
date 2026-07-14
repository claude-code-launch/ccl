package cmd

import (
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunEnvSetValidatesMaxOutputTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "test",
		Providers: map[string]provider.Provider{
			"test": {Name: "test", Type: "openai", Endpoint: "https://example.test/v1", APIKey: "key", Model: "model"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	err := runEnvSet([]string{claude.MaxOutputTokensEnv, "1050000"})
	if err == nil || !strings.Contains(err.Error(), "1 and 128000") {
		t.Fatalf("oversized output token setting error = %v", err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, exists := loaded.Providers["test"].Env[claude.MaxOutputTokensEnv]; exists {
		t.Fatal("invalid max output token value was persisted")
	}

	if err := runEnvSet([]string{claude.MaxOutputTokensEnv, "064000"}); err != nil {
		t.Fatalf("valid max output token setting: %v", err)
	}
	loaded, err = config.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := loaded.Providers["test"].Env[claude.MaxOutputTokensEnv]; got != "64000" {
		t.Fatalf("normalized max output tokens = %q, want 64000", got)
	}
}
