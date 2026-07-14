package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestSaveAndLoadUsesPrivateAtomicConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := &provider.Config{
		ActiveProvider: "gateway",
		Providers: map[string]provider.Provider{
			"gateway": {
				Name:          "gateway",
				Type:          "openai",
				Endpoint:      "https://example.test/v1",
				APIKey:        "secret",
				Model:         "model-a",
				OAuthProvider: "codex",
				SubagentModel: "model-subagent",
			},
		},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(home, ".ccl", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".ccl", ".config-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary config files = %v, err=%v", matches, err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.ActiveProvider != want.ActiveProvider ||
		got.Providers["gateway"].APIKey != "secret" ||
		got.Providers["gateway"].OAuthProvider != "codex" ||
		got.Providers["gateway"].SubagentModel != "model-subagent" {
		t.Fatalf("loaded config = %+v, want %+v", got, want)
	}
}

func TestLoadMigratesLegacyConfigAndSecuresPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyDir := filepath.Join(home, ".cc")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("create legacy dir: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "config.yaml")
	legacyData := []byte("active_provider: legacy\nproviders:\n  legacy:\n    name: legacy\n    type: openai\n    endpoint: https://example.test/v1\n    apikey: secret\n")
	if err := os.WriteFile(legacyPath, legacyData, 0o644); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.ActiveProvider != "legacy" || got.Providers["legacy"].Endpoint != "https://example.test/v1" {
		t.Fatalf("legacy config was not loaded: %+v", got)
	}

	path := filepath.Join(home, ".ccl", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat migrated config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("migrated config permissions = %o, want 600", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy config should be moved, stat err=%v", err)
	}
}

func TestLoadInfersOAuthProviderForLegacyAuthEndpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".ccl")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	data := []byte(`providers:
  chatgpt:
    name: chatgpt
    type: openai_responses
    endpoint: oauth://codex
    model: gpt-5.4-mini
  codex:
    name: codex
    type: openai_responses
    endpoint: oauth://codex
    model: gpt-5.4-mini
  gemini:
    name: gemini
    type: openai
    endpoint: oauth://antigravity
    model: gemini-test
  regular:
    name: regular
    type: openai
    endpoint: https://example.test/v1
    model: gpt-test
`)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := cfg.Providers["chatgpt"].OAuthProvider; got != "chatgpt" {
		t.Fatalf("chatgpt OAuth provider = %q", got)
	}
	if got := cfg.Providers["codex"].OAuthProvider; got != "codex" {
		t.Fatalf("legacy Codex OAuth provider = %q", got)
	}
	if got := cfg.Providers["gemini"].OAuthProvider; got != "gemini" {
		t.Fatalf("Gemini OAuth provider = %q", got)
	}
	if got := cfg.Providers["regular"].OAuthProvider; got != "" {
		t.Fatalf("ordinary provider inferred as OAuth: %q", got)
	}
}
