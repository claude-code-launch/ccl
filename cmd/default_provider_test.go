package cmd

import (
	"bytes"
	"context"
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
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

// TestRunClaudeOnDefaultLaunchesPlainClaude pins what default means: no
// provider is prepared (the ACP one here would fail to), no ccl --settings
// file, the environment passes through, and bypass still applies.
func TestRunClaudeOnDefaultLaunchesPlainClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	saveToggleFixture(t, &provider.Config{
		ActiveProvider: provider.DefaultProvider,
		ACPProvider:    "broken",
		BypassMode:     true,
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
		t.Fatal("a ccl settings file was passed on default")
	}
}

func fakeRunningProxy(t *testing.T, name string, running bool) {
	t.Helper()
	previous := runningProxyProvider
	runningProxyProvider = func(context.Context) (string, bool) { return name, running }
	t.Cleanup(func() { runningProxyProvider = previous })
}

// TestUseDefaultForNormalAndACP covers ccl use default in both modes. A
// running proxy does not block it: default is Claude Code's configuration as
// it stands, proxy overlay included.
func TestUseDefaultForNormalAndACP(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})
	fakeRunningProxy(t, "a", true)
	if err := runProviderUse(provider.DefaultProvider, false); err != nil {
		t.Fatal(err)
	}
	if cfg := loadToggleConfig(t); cfg.ActiveProvider != provider.DefaultProvider {
		t.Fatalf("active = %q", cfg.ActiveProvider)
	}
	if err := runProviderUse("a", true); err != nil {
		t.Fatal(err)
	}
	if err := runProviderUse(provider.DefaultProvider, true); err != nil {
		t.Fatal(err)
	}
	cfg := loadToggleConfig(t)
	if cfg.ACPProvider != provider.DefaultProvider || cfg.EffectiveACPProvider() != provider.DefaultProvider {
		t.Fatalf("acp = %q", cfg.ACPProvider)
	}
	snapshot, err := resolveACPProvider()
	if err != nil || snapshot.name != provider.DefaultProvider {
		t.Fatalf("ACP snapshot = %+v, %v", snapshot, err)
	}
	if err := runProviderUse("missing", false); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

func TestListShowsTheDefaultRow(t *testing.T) {
	cfg := &provider.Config{
		ActiveProvider: provider.DefaultProvider, ACPProvider: "a",
		Providers: map[string]provider.Provider{
			"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
		},
	}
	var out bytes.Buffer
	if err := printProviders(&out, cfg, false, "empty", "Registered providers:"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[2], "default") || !strings.Contains(lines[2], "normal") ||
		!strings.HasPrefix(lines[3], "a ") || !strings.Contains(lines[3], "ACP") {
		t.Fatalf("ls:\n%s", out.String())
	}
	out.Reset()
	if err := printProviders(&out, &provider.Config{ActiveProvider: provider.DefaultProvider}, false, "empty", "Registered providers:"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "default") || !strings.Contains(out.String(), "normal+ACP") || !strings.Contains(out.String(), "empty") {
		t.Fatalf("ls without providers:\n%s", out.String())
	}
}

func TestDefaultIsAReservedProviderName(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})
	if err := runProviderCopy("a", "default", true); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("cp to default: %v", err)
	}
	if err := runProviderMove("a", "default", true); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("mv to default: %v", err)
	}
	if err := validateProviderAlias("default"); err == nil {
		t.Fatal("oauth alias default was accepted")
	}
	if name, collided := providerSaveName(map[string]provider.Provider{}, "", "default"); name != "default-2" || !collided {
		t.Fatalf("set saved default as %q (collided=%v)", name, collided)
	}
}
