package claude_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type settingsJSON struct {
	Env                    map[string]string `json:"env"`
	HasCompletedOnboarding bool              `json:"hasCompletedOnboarding"`
	Model                  string            `json:"model,omitempty"`
	ModelOverrides         map[string]string `json:"modelOverrides,omitempty"`
}

func TestPreviewSettingsFeatures(t *testing.T) {
	tests := []struct {
		name     string
		provider provider.Provider
		check    func(t *testing.T, s settingsJSON)
	}{
		{
			name: "Native Anthropic with explicit tier models",
			provider: provider.Provider{
				Name:        "anthropic-native",
				Type:        "anthropic",
				Endpoint:    "https://api.anthropic.com",
				APIKey:      "sk-test",
				OpusModel:   "claude-opus-4-20250514",
				SonnetModel: "claude-sonnet-4-20250514",
				HaikuModel:  "claude-haiku-3.5-20241022",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "claude-opus-4-20250514" {
					t.Errorf("Opus model mismatch: %s", s.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
				}
				if s.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "claude-sonnet-4-20250514" {
					t.Errorf("Sonnet model mismatch: %s", s.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
				}
				if s.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "claude-haiku-3.5-20241022" {
					t.Errorf("Haiku model mismatch: %s", s.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
				}
			},
		},
		{
			name: "Native Anthropic with model pool",
			provider: provider.Provider{
				Name:     "anthropic-pool",
				Type:     "anthropic",
				Endpoint: "https://api.anthropic.com",
				APIKey:   "sk-test",
				Model:    "claude-opus-4,claude-sonnet-4,claude-haiku-3.5",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] == "" {
					t.Error("Opus model should be set from pool")
				}
				if s.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] == "" {
					t.Error("Sonnet model should be set from pool")
				}
			},
		},
		{
			name: "OpenAI proxy (DeepSeek)",
			provider: provider.Provider{
				Name:     "deepseek",
				Type:     "openai",
				Endpoint: "https://api.deepseek.com",
				APIKey:   "sk-test",
				Model:    "deepseek-reasoner,deepseek-chat",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_BASE_URL"] == "" {
					t.Error("ANTHROPIC_BASE_URL should be set for proxy")
				}
				if s.Env["ANTHROPIC_API_KEY"] != "local-proxy-dummy-key" {
					t.Errorf("API key should be dummy for proxy: %s", s.Env["ANTHROPIC_API_KEY"])
				}
			},
		},
		{
			name: "Custom Model ID (Bedrock ARN)",
			provider: provider.Provider{
				Name:          "bedrock-custom",
				Type:          "anthropic",
				Endpoint:      "https://api.anthropic.com", // Use valid endpoint to avoid fetch
				APIKey:        "sk-test",
				CustomModelID: "arn:aws:bedrock:us-east-1:123456789012:custom-model/my-model",
				Model:         "dummy", // Prevent model fetching
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "arn:aws:bedrock:us-east-1:123456789012:custom-model/my-model" {
					t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION mismatch: %s", s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"])
				}
				if s.Env["CLAUDE_CODE_MODEL_ID"] != "arn:aws:bedrock:us-east-1:123456789012:custom-model/my-model" {
					t.Errorf("CLAUDE_CODE_MODEL_ID mismatch: %s", s.Env["CLAUDE_CODE_MODEL_ID"])
				}
			},
		},
		{
			name: "Model Overrides",
			provider: provider.Provider{
				Name:     "gateway-overrides",
				Type:     "anthropic",
				Endpoint: "https://api.anthropic.com", // Use valid endpoint to avoid fetch
				APIKey:   "sk-test",
				Model:    "dummy", // Prevent model fetching
				ModelOverrides: map[string]string{
					"claude-opus-4-20250514":   "arn:aws:bedrock:...:inference-profile/custom-opus",
					"claude-sonnet-4-20250514": "anthropic/claude-sonnet-4-custom",
				},
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.ModelOverrides == nil {
					t.Error("ModelOverrides should be in settings")
				}
				if s.ModelOverrides["claude-opus-4-20250514"] != "arn:aws:bedrock:...:inference-profile/custom-opus" {
					t.Errorf("ModelOverride mismatch: %v", s.ModelOverrides)
				}
			},
		},
		{
			name: "Effort Level (high)",
			provider: provider.Provider{
				Name:        "effort-test",
				Type:        "anthropic",
				Endpoint:    "https://api.anthropic.com",
				APIKey:      "sk-test",
				EffortLevel: "high",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
					t.Errorf("Effort level mismatch: %s", s.Env["CLAUDE_CODE_EFFORT_LEVEL"])
				}
			},
		},
		{
			name: "Lock Model",
			provider: provider.Provider{
				Name:      "lock-test",
				Type:      "anthropic",
				Endpoint:  "https://api.anthropic.com",
				APIKey:    "sk-test",
				LockModel: "claude-sonnet-4-20250514",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Model != "claude-sonnet-4-20250514" {
					t.Errorf("LockModel mismatch: %s", s.Model)
				}
			},
		},
		{
			name: "Combined: CustomModelID + EffortLevel + LockModel",
			provider: provider.Provider{
				Name:          "combined",
				Type:          "anthropic",
				Endpoint:      "https://api.anthropic.com",
				APIKey:        "sk-test",
				CustomModelID: "my-custom-model",
				EffortLevel:   "high",
				LockModel:     "claude-opus-4",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "my-custom-model" {
					t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION mismatch: %s", s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"])
				}
				if s.Env["CLAUDE_CODE_MODEL_ID"] != "my-custom-model" {
					t.Errorf("CLAUDE_CODE_MODEL_ID mismatch: %s", s.Env["CLAUDE_CODE_MODEL_ID"])
				}
				if s.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
					t.Errorf("Effort level mismatch: %s", s.Env["CLAUDE_CODE_EFFORT_LEVEL"])
				}
				if s.Model != "claude-opus-4" {
					t.Errorf("LockModel mismatch: %s", s.Model)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := claude.PreviewSettings(tt.provider)
			var s settingsJSON
			if err := json.Unmarshal([]byte(result), &s); err != nil {
				t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, result)
			}
			tt.check(t, s)

			// Print for debugging
			data, _ := json.MarshalIndent(s, "", "  ")
			fmt.Printf("\n=== %s ===\n%s\n", tt.name, string(data))
		})
	}
}

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

	var settings settingsJSON
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}

	env := settings.Env
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
			"CLAUDE_CODE_DISABLE_1M_CONTEXT":  "1",
		},
	}

	settingsJSONStr := claude.PreviewSettings(p)

	var settings settingsJSON
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}

	env := settings.Env
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
