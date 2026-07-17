package claude_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
			name: "Native Anthropic with bearer auth",
			provider: provider.Provider{
				Name:          "sensenova",
				Type:          "anthropic",
				Endpoint:      "https://token.sensenova.cn/v1",
				APIKey:        "sk-test",
				Model:         "sensenova-6.7-flash-lite",
				AnthropicAuth: "bearer",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_BASE_URL"] != "https://token.sensenova.cn" {
					t.Errorf("ANTHROPIC_BASE_URL should strip /v1 for Claude Code: %s", s.Env["ANTHROPIC_BASE_URL"])
				}
				if s.Env["ANTHROPIC_AUTH_TOKEN"] != "sk-test" {
					t.Errorf("ANTHROPIC_AUTH_TOKEN mismatch: %s", s.Env["ANTHROPIC_AUTH_TOKEN"])
				}
				if s.Env["ANTHROPIC_API_KEY"] != "" {
					t.Errorf("ANTHROPIC_API_KEY should not be set for bearer auth: %s", s.Env["ANTHROPIC_API_KEY"])
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
				baseURL := s.Env["ANTHROPIC_BASE_URL"]
				if baseURL == "" {
					t.Error("ANTHROPIC_BASE_URL should be set for proxy")
				}
				if strings.HasSuffix(baseURL, "/v1") {
					t.Errorf("Claude Code base URL must not include /v1: %s", baseURL)
				}
				if token := s.Env["ANTHROPIC_AUTH_TOKEN"]; !strings.HasPrefix(token, "ccl-") || token == "sk-test" {
					t.Errorf("auth token should be an isolated CLIProxyAPI session key: %s", token)
				}
				if key := s.Env["ANTHROPIC_API_KEY"]; key != "" {
					t.Errorf("ANTHROPIC_API_KEY should not be set for proxy auth: %s", key)
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
			name: "Combined: CustomModelID + EffortLevel",
			provider: provider.Provider{
				Name:          "combined",
				Type:          "anthropic",
				Endpoint:      "https://api.anthropic.com",
				APIKey:        "sk-test",
				CustomModelID: "my-custom-model",
				EffortLevel:   "high",
			},
			check: func(t *testing.T, s settingsJSON) {
				if s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "my-custom-model" {
					t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION mismatch: %s", s.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"])
				}
				if s.Env["CLAUDE_CODE_MODEL_ID"] != "my-custom-model" {
					t.Errorf("CLAUDE_CODE_MODEL_ID mismatch: %s", s.Env["CLAUDE_CODE_MODEL_ID"])
				}
				if s.Model != "my-custom-model" {
					t.Errorf("top-level model mismatch: %s", s.Model)
				}
				if s.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
					t.Errorf("Effort level mismatch: %s", s.Env["CLAUDE_CODE_EFFORT_LEVEL"])
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
		})
	}
}

func TestPreviewSettingsReservesEmbeddedProxyTransportEnv(t *testing.T) {
	result := claude.PreviewSettings(provider.Provider{
		Name:     "responses-proxy",
		Type:     "openai_responses",
		Endpoint: "https://api.example.com/v1",
		APIKey:   "upstream-key",
		Model:    "gpt-test",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":   "https://stale.example.com",
			"ANTHROPIC_API_KEY":    "stale-api-key",
			"ANTHROPIC_AUTH_TOKEN": "stale-auth-token",
			"CCL_TEST_SENTINEL":    "preserved",
		},
	})
	var settings settingsJSON
	if err := json.Unmarshal([]byte(result), &settings); err != nil {
		t.Fatalf("PreviewSettings() returned invalid JSON: %v; result=%s", err, result)
	}
	if baseURL := settings.Env["ANTHROPIC_BASE_URL"]; baseURL == "https://stale.example.com" || !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Fatalf("proxy base URL = %q, want embedded loopback URL", baseURL)
	}
	if token := settings.Env["ANTHROPIC_AUTH_TOKEN"]; token == "stale-auth-token" || !strings.HasPrefix(token, "ccl-") {
		t.Fatalf("proxy auth token = %q, want isolated ccl session token", token)
	}
	if key := settings.Env["ANTHROPIC_API_KEY"]; key != "" {
		t.Fatalf("proxy API key = %q, want absent", key)
	}
	if got := settings.Env["CCL_TEST_SENTINEL"]; got != "preserved" {
		t.Fatalf("unrelated provider env = %q, want preserved", got)
	}
}

func TestPreviewSettingsKeepsDirectAnthropicAPIKey(t *testing.T) {
	result := claude.PreviewSettings(provider.Provider{
		Name:     "anthropic-direct",
		Type:     "anthropic",
		Endpoint: "https://api.anthropic.com",
		APIKey:   "sk-test",
		Model:    "claude-test",
	})
	var settings settingsJSON
	if err := json.Unmarshal([]byte(result), &settings); err != nil {
		t.Fatalf("PreviewSettings() returned invalid JSON: %v; result=%s", err, result)
	}
	if key := settings.Env["ANTHROPIC_API_KEY"]; key != "sk-test" {
		t.Fatalf("direct Anthropic API key = %q, want sk-test", key)
	}
	if token := settings.Env["ANTHROPIC_AUTH_TOKEN"]; token != "" {
		t.Fatalf("direct Anthropic auth token = %q, want absent", token)
	}
}

func TestPreviewSettingsWithEmbeddedCodexOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credential := []byte(`{"type":"codex","access_token":"test-token","refresh_token":"test-refresh","email":"test@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "codex-test.json"), credential, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	result := claude.PreviewSettings(provider.Provider{
		Name:          "codex",
		Type:          "openai_responses",
		Endpoint:      "oauth://codex",
		OAuthProvider: "codex",
		Model:         "gpt-test",
		CustomModelID: "gpt-test",
	})
	var settings settingsJSON
	if err := json.Unmarshal([]byte(result), &settings); err != nil {
		t.Fatalf("PreviewSettings() returned invalid JSON: %v; result=%s", err, result)
	}
	if settings.Model != "gpt-test" || settings.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "gpt-test" {
		t.Fatalf("OAuth settings = %+v", settings)
	}
	baseURL := settings.Env["ANTHROPIC_BASE_URL"]
	if baseURL == "" || baseURL == "oauth://codex" {
		t.Fatalf("OAuth provider did not use ccl local proxy: %q", baseURL)
	}
	if strings.HasSuffix(baseURL, "/v1") {
		t.Fatalf("OAuth Claude base URL would produce /v1/v1/messages: %q", baseURL)
	}
	if token := settings.Env["ANTHROPIC_AUTH_TOKEN"]; !strings.HasPrefix(token, "ccl-") {
		t.Fatalf("OAuth proxy auth token = %q, want isolated ccl session token", token)
	}
	if key := settings.Env["ANTHROPIC_API_KEY"]; key != "" {
		t.Fatalf("OAuth proxy should not set ANTHROPIC_API_KEY: %q", key)
	}
}

func TestRunSanitizesEmbeddedProxyEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude executable uses a POSIX shell")
	}

	tempDir := t.TempDir()
	fakeClaude := filepath.Join(tempDir, "claude")
	script := `#!/bin/sh
set -eu
env > "$CCL_TEST_ENV_OUTPUT"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--settings" ]; then
    cp "$2" "$CCL_TEST_SETTINGS_OUTPUT"
    exit 0
  fi
  shift
done
exit 2
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", tempDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_API_KEY", "ambient-api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-auth-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ambient.example.com")
	t.Setenv("CCL_TEST_SENTINEL", "preserved")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "preserved-oauth-token")

	readChildEnv := func(path string) map[string]string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read child environment: %v", err)
		}
		env := make(map[string]string)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				env[key] = value
			}
		}
		return env
	}

	providers := []provider.Provider{
		{Name: "chat", Type: "openai", Endpoint: "https://api.example.com/v1", APIKey: "upstream-key", Model: "gpt-test"},
		{Name: "responses", Type: "openai_responses", Endpoint: "https://api.example.com/v1", APIKey: "upstream-key", Model: "gpt-test"},
	}
	for _, p := range providers {
		t.Run(p.Name, func(t *testing.T) {
			var previousToken string
			for run := 1; run <= 2; run++ {
				envOutput := filepath.Join(tempDir, fmt.Sprintf("%s-%d.env", p.Name, run))
				settingsOutput := filepath.Join(tempDir, fmt.Sprintf("%s-%d.json", p.Name, run))
				t.Setenv("CCL_TEST_ENV_OUTPUT", envOutput)
				t.Setenv("CCL_TEST_SETTINGS_OUTPUT", settingsOutput)

				if err := claude.Run(p, nil); err != nil {
					t.Fatalf("Run() attempt %d: %v", run, err)
				}
				childEnv := readChildEnv(envOutput)
				if _, ok := childEnv["ANTHROPIC_API_KEY"]; ok {
					t.Fatalf("attempt %d inherited ANTHROPIC_API_KEY", run)
				}
				token := childEnv["ANTHROPIC_AUTH_TOKEN"]
				if !strings.HasPrefix(token, "ccl-") || token == "ambient-auth-token" {
					t.Fatalf("attempt %d auth token = %q, want isolated ccl token", run, token)
				}
				baseURL := childEnv["ANTHROPIC_BASE_URL"]
				if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
					t.Fatalf("attempt %d base URL = %q, want embedded loopback URL", run, baseURL)
				}
				if childEnv["CCL_TEST_SENTINEL"] != "preserved" || childEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "preserved-oauth-token" {
					t.Fatalf("attempt %d did not preserve unrelated environment", run)
				}

				data, err := os.ReadFile(settingsOutput)
				if err != nil {
					t.Fatalf("read copied settings: %v", err)
				}
				var settings settingsJSON
				if err := json.Unmarshal(data, &settings); err != nil {
					t.Fatalf("decode copied settings: %v", err)
				}
				if settings.Env["ANTHROPIC_AUTH_TOKEN"] != token || settings.Env["ANTHROPIC_BASE_URL"] != baseURL {
					t.Fatalf("attempt %d process and settings transport values differ", run)
				}
				if _, ok := settings.Env["ANTHROPIC_API_KEY"]; ok {
					t.Fatalf("attempt %d settings contain ANTHROPIC_API_KEY", run)
				}
				if previousToken != "" && previousToken == token {
					t.Fatalf("repeated launch reused session token %q", token)
				}
				previousToken = token
			}
		})
	}
}

func TestPreviewSettingsRejectsAnthropicOAuthProvider(t *testing.T) {
	result := claude.PreviewSettings(provider.Provider{
		Name:          "codex",
		Type:          "anthropic",
		OAuthProvider: "codex",
	})
	if !strings.Contains(result, "requires the OpenAI Chat or Responses protocol") {
		t.Fatalf("PreviewSettings() = %q, want OAuth protocol validation error", result)
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

func TestPreviewSettingsOmitsDefaultEffortAndTopLevelModel(t *testing.T) {
	p := provider.Provider{
		Name:     "default-effort",
		Type:     "anthropic",
		Endpoint: "https://api.anthropic.com",
		APIKey:   "sk-test",
		Model:    "claude-sonnet-4",
	}

	settingsJSONStr := claude.PreviewSettings(p)

	var raw map[string]any
	if err := json.Unmarshal([]byte(settingsJSONStr), &raw); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}
	if _, ok := raw["model"]; ok {
		t.Fatalf("settings JSON should not include top-level model lock: %s", settingsJSONStr)
	}

	env, ok := raw["env"].(map[string]any)
	if !ok {
		t.Fatalf("No env block found in settings: %s", settingsJSONStr)
	}
	if _, ok := env["CLAUDE_CODE_EFFORT_LEVEL"]; ok {
		t.Fatalf("default effort should not inject CLAUDE_CODE_EFFORT_LEVEL: %s", settingsJSONStr)
	}
}

func TestPreviewSettingsSingleModelPoolFillsDefaultSlots(t *testing.T) {
	p := provider.Provider{
		Name:     "single-model-pool",
		Type:     "anthropic",
		Endpoint: "https://api.anthropic.com",
		APIKey:   "sk-test",
		Model:    "sensenova-u1-fast",
	}

	settingsJSONStr := claude.PreviewSettings(p)

	var settings settingsJSON
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_MODEL",
	} {
		if settings.Env[key] != "sensenova-u1-fast" {
			t.Fatalf("expected %s to use the single model pool value, got %q", key, settings.Env[key])
		}
	}
}

func TestPreviewSettingsAppliesRuntimeDefaults(t *testing.T) {
	p := provider.Provider{
		Name:          "runtime-defaults",
		Type:          "anthropic",
		Endpoint:      "https://api.anthropic.com",
		APIKey:        "sk-test",
		Model:         "gpt-5.6-sol,gpt-5.6-mini",
		CustomModelID: "gpt-5.6-sol",
	}

	var settings settingsJSON
	if err := json.Unmarshal([]byte(claude.PreviewSettings(p)), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings.Env[claude.SubagentModelEnv] != "gpt-5.6-sol" {
		t.Fatalf("subagent model = %q", settings.Env[claude.SubagentModelEnv])
	}
	if settings.Env[claude.ToolUseConcurrencyEnv] != claude.DefaultToolUseConcurrency {
		t.Fatalf("tool concurrency = %q", settings.Env[claude.ToolUseConcurrencyEnv])
	}
	if settings.Env[claude.ToolSearchEnv] != claude.DefaultToolSearch {
		t.Fatalf("tool search = %q", settings.Env[claude.ToolSearchEnv])
	}
	if settings.Env[claude.MaxOutputTokensEnv] != claude.DefaultMaxOutputTokens {
		t.Fatalf("max output tokens = %q", settings.Env[claude.MaxOutputTokensEnv])
	}
}

func TestPreviewSettingsAppliesExplicitSubagentMapping(t *testing.T) {
	p := provider.Provider{
		Name:          "subagent-mapping",
		Type:          "anthropic",
		Endpoint:      "https://api.anthropic.com",
		APIKey:        "sk-test",
		CustomModelID: "main-model",
		SubagentModel: "cheap-subagent-model",
	}

	var settings settingsJSON
	if err := json.Unmarshal([]byte(claude.PreviewSettings(p)), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got := settings.Env[claude.SubagentModelEnv]; got != "cheap-subagent-model" {
		t.Fatalf("subagent model = %q, want explicit mapping", got)
	}
}

func TestPreviewSettingsRuntimeDefaultsCanBeOverridden(t *testing.T) {
	p := provider.Provider{
		Name:          "runtime-overrides",
		Type:          "anthropic",
		Endpoint:      "https://api.anthropic.com",
		APIKey:        "sk-test",
		SonnetModel:   "default-sonnet",
		SubagentModel: "mapped-subagent",
		Env: map[string]string{
			claude.SubagentModelEnv:      "override-subagent",
			claude.ToolUseConcurrencyEnv: "7",
			claude.ToolSearchEnv:         "true",
			claude.MaxOutputTokensEnv:    "64000",
		},
	}

	var settings settingsJSON
	if err := json.Unmarshal([]byte(claude.PreviewSettings(p)), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings.Env[claude.SubagentModelEnv] != "override-subagent" ||
		settings.Env[claude.ToolUseConcurrencyEnv] != "7" ||
		settings.Env[claude.ToolSearchEnv] != "true" ||
		settings.Env[claude.MaxOutputTokensEnv] != "64000" {
		t.Fatalf("runtime overrides not applied: %+v", settings.Env)
	}
}

func TestPreviewSettingsRejectsOversizedMaxOutputTokenOverride(t *testing.T) {
	p := provider.Provider{
		Type:     "anthropic",
		Endpoint: "https://api.anthropic.com",
		APIKey:   "sk-test",
		Env: map[string]string{
			claude.MaxOutputTokensEnv: "1050000",
		},
	}

	var settings settingsJSON
	if err := json.Unmarshal([]byte(claude.PreviewSettings(p)), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got := settings.Env[claude.MaxOutputTokensEnv]; got != claude.DefaultMaxOutputTokens {
		t.Fatalf("oversized max output tokens resolved to %q, want %q", got, claude.DefaultMaxOutputTokens)
	}
}

func TestPreviewSettingsModelPoolDoesNotOverrideExplicitSlots(t *testing.T) {
	p := provider.Provider{
		Name:        "partial-explicit-slots",
		Type:        "anthropic",
		Endpoint:    "https://api.anthropic.com",
		APIKey:      "sk-test",
		Model:       "claude-opus-auto,claude-sonnet-auto,claude-haiku-auto",
		OpusModel:   "manual-opus",
		HaikuModel:  "manual-haiku",
		SonnetModel: "",
	}

	settingsJSONStr := claude.PreviewSettings(p)

	var settings settingsJSON
	if err := json.Unmarshal([]byte(settingsJSONStr), &settings); err != nil {
		t.Fatalf("Failed to parse settings JSON: %v. JSON: %s", err, settingsJSONStr)
	}
	if settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "manual-opus" {
		t.Fatalf("expected explicit opus slot to be preserved, got %q", settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "manual-haiku" {
		t.Fatalf("expected explicit haiku slot to be preserved, got %q", settings.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
	if settings.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "claude-sonnet-auto" {
		t.Fatalf("expected missing sonnet slot to be filled from model pool, got %q", settings.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
	}
	if settings.Env["ANTHROPIC_MODEL"] != "claude-sonnet-auto" {
		t.Fatalf("expected ANTHROPIC_MODEL to follow final sonnet fallback, got %q", settings.Env["ANTHROPIC_MODEL"])
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

func TestPreviewSettingsUsesDisplayNameWithoutTechnicalSuffix(t *testing.T) {
	result := claude.PreviewSettings(provider.Provider{
		Type:          "anthropic",
		Endpoint:      "https://example.test",
		APIKey:        "k",
		OpusModel:     "gpt-5.5[1m]",
		SonnetModel:   "grok-4.5[1m]",
		HaikuModel:    "mini",
		CustomModelID: "custom[1m]",
	})
	var settings settingsJSON
	if err := json.Unmarshal([]byte(result), &settings); err != nil {
		t.Fatalf("PreviewSettings() returned invalid JSON: %v; result=%s", err, result)
	}
	if settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "gpt-5.5[1m]" {
		t.Fatalf("opus technical id = %q", settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] != "gpt-5.5 (1M)" {
		t.Fatalf("opus display = %q", settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"])
	}
	if settings.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "custom[1m]" {
		t.Fatalf("custom technical id = %q", settings.Env["ANTHROPIC_CUSTOM_MODEL_OPTION"])
	}
	if settings.Env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"] != "custom (1M)" {
		t.Fatalf("custom display = %q", settings.Env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"])
	}
}
