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

}

// A fresh install has no provider yet, so normal and ACP launches use Claude
// Code's own configuration: the "default" selector.
func TestFreshInstallSelectsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg, err := Load()
	if err != nil || cfg.ActiveProvider != provider.DefaultProvider || cfg.EffectiveACPProvider() != provider.DefaultProvider {
		t.Fatalf("fresh install: active=%q acp=%q err=%v", cfg.ActiveProvider, cfg.EffectiveACPProvider(), err)
	}
	if err := Update(func(c *provider.Config) error { c.Lang = "en"; return nil }); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(); cfg.ActiveProvider != provider.DefaultProvider {
		t.Fatalf("first write dropped default: %q", cfg.ActiveProvider)
	}
	writeRawConfig(t, "active_provider: a\nproviders:\n  a:\n    type: openai\n    endpoint: https://example.com/v1\n")
	if cfg, _ := Load(); cfg.ActiveProvider != "a" {
		t.Fatalf("an existing config lost its provider: %q", cfg.ActiveProvider)
	}
}
