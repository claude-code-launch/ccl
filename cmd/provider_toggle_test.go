package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func saveToggleFixture(t *testing.T, cfg *provider.Config) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if cfg.Providers == nil {
		cfg.Providers = map[string]provider.Provider{}
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func loadToggleConfig(t *testing.T) *provider.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProviderToggleRoundTrip(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})

	var out bytes.Buffer
	if err := runProviderToggle(&out, "off"); err != nil {
		t.Fatal(err)
	}
	if !loadToggleConfig(t).ProviderOff {
		t.Fatal("provider_off was not saved")
	}
	if !strings.Contains(out.String(), "off") {
		t.Fatalf("off output = %q", out.String())
	}

	out.Reset()
	if err := runProviderToggle(&out, "on"); err != nil {
		t.Fatal(err)
	}
	cfg := loadToggleConfig(t)
	if cfg.ProviderOff {
		t.Fatal("provider_off is still set after on")
	}
	// The selections are untouched by the toggle.
	if cfg.ActiveProvider != "a" {
		t.Fatalf("active provider changed: %q", cfg.ActiveProvider)
	}
	if !strings.Contains(out.String(), "on") || !strings.Contains(out.String(), "a") {
		t.Fatalf("on output = %q", out.String())
	}

	if err := runProviderToggle(&out, "maybe"); err == nil {
		t.Fatal("an unknown action was accepted")
	}
}

// TestProviderOffIsOmittedFromConfigWhenOn keeps existing configs unchanged:
// the field only appears in config.yaml while loading is off.
func TestProviderOffIsOmittedFromConfigWhenOn(t *testing.T) {
	saveToggleFixture(t, &provider.Config{})
	path := filepath.Join(os.Getenv("HOME"), ".ccl", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "provider_off") {
		t.Fatalf("provider_off written while on:\n%s", data)
	}
	if err := runProviderToggle(&bytes.Buffer{}, "off"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "provider_off: true") {
		t.Fatalf("provider_off missing while off:\n%s", data)
	}
}

// TestRunClaudeWithProviderOffLaunchesPlainClaude pins what off means: no
// provider is prepared (the active one here would fail to), no ccl --settings
// file, the environment passes through, and bypass still applies.
func TestRunClaudeWithProviderOffLaunchesPlainClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	saveToggleFixture(t, &provider.Config{
		ActiveProvider: "broken",
		BypassMode:     true,
		ProviderOff:    true,
		Providers: map[string]provider.Provider{
			"broken": {Name: "broken", Type: "openai", Endpoint: "://invalid"},
		},
	})

	bin := t.TempDir()
	record := filepath.Join(bin, "record")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do printf 'arg=%s\\n' \"$arg\" >> '" + record + "'; done\n" +
		"printf 'base=%s\\n' \"$ANTHROPIC_BASE_URL\" >> '" + record + "'\n" +
		"printf 'key=%s\\n' \"$ANTHROPIC_API_KEY\" >> '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the fake is on PATH, so a real Claude Code can never start here.
	t.Setenv("PATH", bin)
	t.Setenv("ANTHROPIC_BASE_URL", "https://kept.example")
	t.Setenv("ANTHROPIC_API_KEY", "kept-key")

	if err := runClaude([]string{"-p", "hello"}); err != nil {
		t.Fatalf("runClaude() = %v", err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "arg=--dangerously-skip-permissions\narg=-p\narg=hello\nbase=https://kept.example\nkey=kept-key\n"
	if got != want {
		t.Fatalf("fake claude saw:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "--settings") {
		t.Fatal("a ccl settings file was passed while provider loading is off")
	}
}

// TestProviderUseTurnsLoadingBackOn covers the agreed shortcut: picking a
// normal-mode provider turns loading on; picking one for ACP does not.
func TestProviderUseTurnsLoadingBackOn(t *testing.T) {
	providers := map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
		"b": {Name: "b", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", ProviderOff: true, Providers: providers})

	if err := runProviderUse("b", true); err != nil {
		t.Fatal(err)
	}
	if cfg := loadToggleConfig(t); !cfg.ProviderOff || cfg.ACPProvider != "b" {
		t.Fatalf("ACP selection changed provider loading: off=%t acp=%q", cfg.ProviderOff, cfg.ACPProvider)
	}

	if err := runProviderUse("b", false); err != nil {
		t.Fatal(err)
	}
	if cfg := loadToggleConfig(t); cfg.ProviderOff || cfg.ActiveProvider != "b" {
		t.Fatalf("normal selection: off=%t active=%q", cfg.ProviderOff, cfg.ActiveProvider)
	}
}

func TestListShowsProviderLoadingOff(t *testing.T) {
	cfg := &provider.Config{
		ActiveProvider: "a", ACPProvider: "a", ProviderOff: true,
		Providers: map[string]provider.Provider{
			"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
		},
	}
	var out bytes.Buffer
	if err := printProviders(&out, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "normal(off)+ACP") {
		t.Fatalf("USED BY does not show off:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ccl provider on") {
		t.Fatalf("no hint to turn loading back on:\n%s", out.String())
	}

	cfg.ProviderOff = false
	out.Reset()
	if err := printProviders(&out, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "off") {
		t.Fatalf("on state mentions off:\n%s", out.String())
	}
}
