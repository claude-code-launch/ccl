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
				OAuthProvider: "gpt",
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
		got.Providers["gateway"].OAuthProvider != "gpt" ||
		got.Providers["gateway"].SubagentModel != "model-subagent" {
		t.Fatalf("loaded config = %+v, want %+v", got, want)
	}
}

func TestLoadDefaultsLogLevelToOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "off" {
		t.Fatalf("default LogLevel = %q, want off", cfg.LogLevel)
	}
}

func TestLoadDerivesTheOAuthProtocol(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"chatgpt": {Name: "chatgpt", Type: "openai", OAuthProvider: "gpt", Endpoint: "oauth://codex"},
			"gemini":  {Name: "gemini", Type: "openai_responses", OAuthProvider: "gemini", Endpoint: "oauth://antigravity"},
		},
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["chatgpt"].Type != "openai_responses" {
		t.Fatalf("chatgpt type = %q", loaded.Providers["chatgpt"].Type)
	}
	if loaded.Providers["gemini"].Type != "openai" {
		t.Fatalf("gemini type = %q", loaded.Providers["gemini"].Type)
	}
	// Second load should be stable.
	loaded2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded2.Providers["chatgpt"].Type != "openai_responses" {
		t.Fatalf("chatgpt type unstable: %q", loaded2.Providers["chatgpt"].Type)
	}
}
