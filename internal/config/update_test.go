package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func writeRawConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".ccl", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadNeverWrites pins S1: reading the config — which ls, preview, doctor
// and ACP do constantly — must not rewrite it, even when it needs migrating.
func TestLoadNeverWrites(t *testing.T) {
	path := writeRawConfig(t, "debug_mode: true\nactive_provider: a\nproviders:\n  a:\n    name: a\n    type: openai-chat\n    oauthProvider: chatgpt\n")
	before, _ := os.ReadFile(path)
	for range 3 {
		if _, err := Load(); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatalf("Load rewrote the config:\n%s", after)
	}
}

// TestConcurrentUpdatesDoNotLoseChanges pins the lost-update fix: each Update
// re-reads the file under the lock, so concurrent writers compose.
func TestConcurrentUpdatesDoNotLoseChanges(t *testing.T) {
	writeRawConfig(t, "providers: {}\n")
	var wg sync.WaitGroup
	const writers = 16
	for i := range writers {
		wg.Go(func() {
			name := fmt.Sprintf("p%02d", i)
			if err := Update(func(cfg *provider.Config) error {
				cfg.Providers[name] = provider.Provider{Name: name, Type: "openai"}
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != writers {
		t.Fatalf("got %d providers after %d concurrent updates", len(cfg.Providers), writers)
	}
}

func TestUpdateErrorLeavesTheFileUntouched(t *testing.T) {
	path := writeRawConfig(t, "active_provider: keep\nproviders: {}\n")
	before, _ := os.ReadFile(path)
	if err := Update(func(cfg *provider.Config) error {
		cfg.ActiveProvider = "changed"
		return fmt.Errorf("abort")
	}); err == nil {
		t.Fatal("Update swallowed the callback error")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a failed Update wrote the config")
	}
}

// TestLegacyGrokDefaultsMigrateOnce pins S13: an older ccl's generated Grok
// defaults move to today's once; a user who then picks grok-4.5 keeps it.
func TestLegacyGrokDefaultsMigrateOnce(t *testing.T) {
	writeRawConfig(t, `providers:
  grok:
    name: grok
    type: openai_responses
    oauthProvider: grok
    endpoint: oauth://grok
    opusModel: grok-4.5
    sonnetModel: grok-4.3
    haikuModel: grok-3-mini
`)
	if err := Migrate(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load()
	if got := cfg.Providers["grok"]; got.OpusModel != "grok-4.6" || got.SonnetModel != "grok-4.5" || got.HaikuModel != "grok-4.5" {
		t.Fatalf("legacy defaults were not migrated: %+v", got)
	}

	if err := Update(func(cfg *provider.Config) error {
		p := cfg.Providers["grok"]
		p.OpusModel = "grok-4.5"
		cfg.Providers["grok"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load()
	if got := cfg.Providers["grok"].OpusModel; got != "grok-4.5" {
		t.Fatalf("a deliberate grok-4.5 choice was migrated again: %q", got)
	}
}

func TestLoadNormalizesTypesAndContextPreset(t *testing.T) {
	writeRawConfig(t, `providers:
  gw:
    name: gw
    type: openai-responses
    endpoint: https://gw.example/v1
    env:
      CLAUDE_CODE_MAX_CONTEXT_TOKENS: "500000"
      CLAUDE_CODE_AUTO_COMPACT_WINDOW: "500000"
      CLAUDE_AUTOCOMPACT_PCT_OVERRIDE: "80"
      CCL_CONTEXT_BUDGET: manual
      KEEP: "1"
  chat:
    name: chat
    type: openai(chat)
    endpoint: https://chat.example/v1
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	gw := cfg.Providers["gw"]
	if gw.Type != "openai_responses" || cfg.Providers["chat"].Type != "openai" {
		t.Fatalf("types = %q / %q", gw.Type, cfg.Providers["chat"].Type)
	}
	if gw.ContextPreset != provider.ContextPresetNameBalanced500K {
		t.Fatalf("legacy Balanced triplet -> preset %q", gw.ContextPreset)
	}
	if len(gw.Env) != 1 || gw.Env["KEEP"] != "1" {
		t.Fatalf("env after normalization = %+v", gw.Env)
	}
	if got := provider.ContextPresetEnv(gw)[provider.EnvAutoCompactPct]; got != provider.Balanced500KAutoCompactPct {
		t.Fatalf("preset launches with pct %q", got)
	}
}

// TestOAuthTypeIsDerivedNotPersisted pins S3: a subscription's type is not
// stored, Load fills it from the backend, and a clean file is not rewritten
// on every startup.
func TestOAuthTypeIsDerivedNotPersisted(t *testing.T) {
	path := writeRawConfig(t, "providers: {}\n")
	if err := Update(func(cfg *provider.Config) error {
		cfg.Providers["g"] = provider.Provider{Name: "g", OAuthProvider: "grok", Type: "openai_responses"}
		cfg.Providers["h"] = provider.Provider{Name: "h", Type: "openai"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	raw := string(data)
	if strings.Count(raw, "type:") != 1 || !strings.Contains(raw, "type: openai\n") {
		t.Fatalf("only the manual gateway should persist its type:\n%s", raw)
	}
	cfg, _ := Load()
	if cfg.Providers["g"].Type != "openai_responses" {
		t.Fatalf("loaded OAuth type = %q", cfg.Providers["g"].Type)
	}

	info, _ := os.Stat(path)
	if err := Migrate(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("Migrate rewrote an already-current file")
	}

	// A stale persisted type (from an older ccl) is a real change.
	writeRawConfig(t, "config_version: 2\nproviders:\n  g:\n    name: g\n    oauthProvider: grok\n    type: openai\n")
	if _, changed, err := read(); err != nil || !changed {
		t.Fatalf("stale OAuth type not reported as a change: %t, %v", changed, err)
	}
}
