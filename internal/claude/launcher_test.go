package claude_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/haiboyuwen/claude-code-launch/internal/claude"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
)

func TestLauncherDynamicDiscovery(t *testing.T) {
	// Start an OpenAI-style mock server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{"id": "deepseek-v4-pro", "object": "model"},
				{"id": "deepseek-v4-flash", "object": "model"}
			]
		}`))
	})

	mockServer := &http.Server{
		Addr:    "127.0.0.1:4569",
		Handler: mux,
	}

	go func() {
		_ = mockServer.ListenAndServe()
	}()
	defer mockServer.Shutdown(context.Background())

	time.Sleep(100 * time.Millisecond)

	// Build a provider where Model is completely empty (relying on dynamic discovery)
	p := provider.Provider{
		Name:     "dynamic-openai",
		Type:     "openai",
		Endpoint: "http://127.0.0.1:4569/v1",
		APIKey:   "mock-key",
		Model:    "", // Empty!
	}

	// PreviewSettings should trigger proxy starting, synchronous discovery, and populate Model
	settingsJSONStr := claude.PreviewSettings(p)

	var settings map[string]map[string]string
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}

	env := settings["env"]
	if env == nil {
		t.Fatalf("No env block found in settings: %s", settingsJSONStr)
	}

	// We expect gateway discovery enabled and correct default models mapped!
	if env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "1" {
		t.Errorf("Expected CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY to be 1")
	}

	// Sonnet model tier should be mapped to deepseek-v4-pro
	if env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "deepseek-v4-pro" {
		t.Errorf("Expected default sonnet model to be 'deepseek-v4-pro', got: %q", env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
	}

	// Haiku model tier should be mapped to deepseek-v4-flash
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "deepseek-v4-flash" {
		t.Errorf("Expected default haiku model to be 'deepseek-v4-flash', got: %q", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
}

func TestLauncherCustomEnv(t *testing.T) {
	p := provider.Provider{
		Name:     "custom-env-test",
		Type:     "anthropic",
		Endpoint: "https://api.anthropic.com",
		APIKey:   "mock-key",
		Model:    "claude-3-5-sonnet",
		Env: map[string]string{
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50",
			"CLAUDE_CODE_DISABLE_1M_CONTEXT": "1",
		},
	}

	settingsJSONStr := claude.PreviewSettings(p)

	var settings map[string]map[string]string
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}

	env := settings["env"]
	if env == nil {
		t.Fatalf("No env block found in settings: %s", settingsJSONStr)
	}

	if env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] != "50" {
		t.Errorf("Expected CLAUDE_AUTOCOMPACT_PCT_OVERRIDE to be 50, got: %q", env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"])
	}

	if env["CLAUDE_CODE_DISABLE_1M_CONTEXT"] != "1" {
		t.Errorf("Expected CLAUDE_CODE_DISABLE_1M_CONTEXT to be 1, got: %q", env["CLAUDE_CODE_DISABLE_1M_CONTEXT"])
	}
}
