package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRunEnvSetPreservesEnvironmentVariableValue(t *testing.T) {
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

	const key = "CUSTOM_GATEWAY_FLAG"
	if err := runEnvSet([]string{key, "1050000"}); err != nil {
		t.Fatalf("set ordinary environment variable: %v", err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := loaded.Providers["test"].Env[key]; got != "1050000" {
		t.Fatalf("stored value = %q, want unchanged", got)
	}
}
