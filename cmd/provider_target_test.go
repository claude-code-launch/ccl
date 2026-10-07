package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestProviderTargetSelectsTheRequestedProvider(t *testing.T) {
	cfg := &provider.Config{ActiveProvider: "a", ACPProvider: "b", Providers: map[string]provider.Provider{
		"a": {Name: "a"}, "b": {Name: "b"}, "c": {Name: "c"},
	}}
	for name, testCase := range map[string]struct {
		target providerTarget
		want   string
	}{
		"default is the active provider": {providerTarget{}, "a"},
		"--acp is ACP's provider":        {providerTarget{acp: true}, "b"},
		"--provider names one":           {providerTarget{name: "c"}, "c"},
	} {
		got, err := testCase.target.resolve(cfg)
		if err != nil || got != testCase.want {
			t.Fatalf("%s: resolve() = %q, %v", name, got, err)
		}
	}
	for name, target := range map[string]providerTarget{
		"unknown provider": {name: "zzz"},
		"both selectors":   {name: "c", acp: true},
	} {
		if _, err := target.resolve(cfg); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := (providerTarget{}).resolve(&provider.Config{Providers: cfg.Providers}); err == nil {
		t.Fatal("no selection resolved to a provider")
	}
}

// TestEnvSetTargetsAndRefusesManagedKeys covers S10 (any provider, not only
// the active one) and R5 (ccl-managed settings are not raw env variables).
func TestEnvSetTargetsAndRefusesManagedKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai"}, "b": {Name: "b", Type: "openai"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := runEnvSet([]string{"FOO", "1"}, providerTarget{name: "b"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.Providers["b"].Env["FOO"] != "1" || cfg.Providers["a"].Env["FOO"] != "" {
		t.Fatalf("--provider b wrote %+v / %+v", cfg.Providers["a"].Env, cfg.Providers["b"].Env)
	}

	for _, key := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "anthropic_default_fable_model", "CLAUDE_CODE_SUBAGENT_MODEL",
		provider.EnvMaxContextTokens, provider.EnvAutoCompactPct, "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
	} {
		err := runEnvSet([]string{key, "x"}, providerTarget{})
		if err == nil || !strings.Contains(err.Error(), "ccl") {
			t.Fatalf("managed key %s: %v", key, err)
		}
	}
	if err := runEnvMove("FOO", "ANTHROPIC_DEFAULT_HAIKU_MODEL", true, providerTarget{name: "b"}); err == nil {
		t.Fatal("renaming onto a managed key was accepted")
	}
}

func useClaudeBinary(t *testing.T, content []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := claudeBinaryPath
	claudeBinaryPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { claudeBinaryPath = previous })
}

func TestClaudeReadsEnvVarScansTheInstalledBinary(t *testing.T) {
	useClaudeBinary(t, []byte("\x00process.env.CLAUDE_CODE_SUBAGENT_MODEL\x00API_TIMEOUT_MS\x00"))
	if read, known := claudeReadsEnvVar("API_TIMEOUT_MS"); !read || !known {
		t.Fatalf("present var: read=%t known=%t", read, known)
	}
	if read, known := claudeReadsEnvVar("CLAUDE_CODE_REASONING_EFFORT"); read || !known {
		t.Fatalf("absent var: read=%t known=%t", read, known)
	}

	// A name split across the 4 MiB read boundary is still found.
	needle := "CLAUDE_CODE_SPLIT_ACROSS_CHUNKS"
	big := append(bytes.Repeat([]byte{'.'}, (4<<20)-10), []byte(needle)...)
	useClaudeBinary(t, big)
	if read, _ := claudeReadsEnvVar(needle); !read {
		t.Fatal("a name across the chunk boundary was missed")
	}

	previous := claudeBinaryPath
	claudeBinaryPath = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { claudeBinaryPath = previous })
	if _, known := claudeReadsEnvVar("ANYTHING"); known {
		t.Fatal("no binary must mean unknown, not \"not read\"")
	}
}

// TestRealClaudeReadsKnownVars is a smoke test against the installed Claude
// Code when there is one (skipped otherwise).
func TestRealClaudeReadsKnownVars(t *testing.T) {
	if _, err := claudeBinaryPath(); err != nil {
		t.Skip("claude is not installed")
	}
	if read, known := claudeReadsEnvVar("ANTHROPIC_DEFAULT_OPUS_MODEL"); known && !read {
		t.Fatal("the installed Claude Code does not mention ANTHROPIC_DEFAULT_OPUS_MODEL")
	}
	if read, known := claudeReadsEnvVar("CLAUDE_CODE_REASONING_EFFORT_NOT_A_REAL_VAR"); known && read {
		t.Fatal("a made-up variable was reported as read")
	}
}
