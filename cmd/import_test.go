package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
)

// isolateAutoClawDesktopAuth points the platform's desktop-login lookup at the
// test's own HOME. Leaving XDG_CONFIG_HOME or APPDATA set would send the real
// resolution outside it: the fixture below would land in the temp directory
// while the import looked at the runner's own config directory.
func isolateAutoClawDesktopAuth(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	return home
}

// writeAutoClawDesktopAuth seeds the AutoClaw desktop auth.json that
// `ccl import autoclaw` reads, inside the isolated HOME of a test. The location
// comes from the same resolver the import uses, so the two cannot disagree.
func writeAutoClawDesktopAuth(t *testing.T, home, accessToken string) {
	t.Helper()
	path, err := oauthproxy.AutoClawDesktopAuthPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, home+string(os.PathSeparator)) {
		t.Fatalf("desktop auth path %q escaped the test HOME %q", path, home)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{
		"deviceId":     "device-import-1",
		"updatedAt":    123,
		"token":        accessToken,
		"refreshToken": "imported-refresh-token",
		"userInfo": map[string]any{
			"user_id":   "user-1",
			"user_name": "Claw",
			"email":     "claw@example.com",
		},
	}
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAuthCredential(t *testing.T, home, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".ccl", "auth", name))
	if err != nil {
		t.Fatalf("read credential %s: %v", name, err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("parse credential %s: %v", name, err)
	}
	return metadata
}

func TestRunImportAutoClawCreatesProvider(t *testing.T) {
	home := isolateAutoClawDesktopAuth(t)
	writeAutoClawDesktopAuth(t, home, "imported-access-token")

	var out bytes.Buffer
	if err := runImport(context.Background(), &out, []string{"autoclaw"}); err != nil {
		t.Fatalf("runImport() error: %v", err)
	}
	if !strings.Contains(out.String(), `Imported autoclaw credential as provider "autoclaw-`) {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "Protocol: openai-chat / autoclaw (fixed for this backend)") {
		t.Fatalf("output = %q", out.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// A bare import derives the provider name from the credential file, so
	// multiple coding-plan accounts never overwrite one another.
	if !strings.HasPrefix(cfg.ActiveProvider, "autoclaw-") {
		t.Fatalf("active provider = %q", cfg.ActiveProvider)
	}
	p, ok := cfg.Providers[cfg.ActiveProvider]
	if !ok {
		t.Fatalf("provider not created: %+v", cfg.Providers)
	}
	if p.Type != "autoclaw" || p.OAuthProvider != "autoclaw" {
		t.Fatalf("AutoClaw provider = %+v", p)
	}
	if p.Endpoint != "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw" {
		t.Fatalf("endpoint = %q", p.Endpoint)
	}
	// The coding-plan key stays out of the plain-text config; the provider only
	// binds the credential file.
	if p.APIKey != "" {
		t.Fatalf("provider stored the API key in config.yaml: %q", p.APIKey)
	}
	if p.OAuthAccountCredential == "" {
		t.Fatal("provider is not bound to a credential file")
	}
	credential := readAuthCredential(t, home, p.OAuthAccountCredential)
	if credential["access_token"] != "imported-access-token" {
		t.Fatalf("credential access_token = %v", credential["access_token"])
	}
	if credential["refresh_token"] != "imported-refresh-token" || credential["type"] != "autoclaw" || credential["source"] != "autoclaw_desktop" {
		t.Fatalf("credential metadata = %+v", credential)
	}
	if credential["source_file"] != "auth.json" || credential["device_id"] != "device-import-1" {
		t.Fatalf("desktop auth metadata = %+v", credential)
	}
	info, err := os.Stat(filepath.Join(home, ".ccl", "auth", p.OAuthAccountCredential))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRunImportAliasBecomesProviderName(t *testing.T) {
	home := isolateAutoClawDesktopAuth(t)
	writeAutoClawDesktopAuth(t, home, "imported-access-token")

	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"autoclaw", "work"}); err != nil {
		t.Fatalf("runImport() error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["work"]
	if !ok || cfg.ActiveProvider != "work" {
		t.Fatalf("alias provider = %+v, active = %q", cfg.Providers, cfg.ActiveProvider)
	}
	if p.OAuthAccountCredential == "" {
		t.Fatalf("credential binding = %q", p.OAuthAccountCredential)
	}
}

func TestRunImportRejectsUnsupportedSourceAndReservedAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The real ImportCredential dispatch rejects non-autoclaw sources before
	// touching HOME or the network.
	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"gpt"}); err == nil {
		t.Fatal("runImport(gpt) should fail")
	}
	// A reserved alias fails before any import work runs.
	if err := runImport(context.Background(), &bytes.Buffer{}, []string{"autoclaw", "gpt"}); err == nil {
		t.Fatal("runImport(autoclaw gpt) should fail on the reserved alias")
	}
}

func TestRunAuthAutoClawRidesTheLoginPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	loginCalls := 0
	original := oauthLogin
	oauthLogin = func(_ context.Context, target string, opts oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		loginCalls++
		if target != oauthproxy.ProviderAutoClaw {
			t.Fatalf("oauthLogin target = %q, want autoclaw", target)
		}
		if opts.NoBrowser {
			t.Fatal("oauthLogin must not force the no-browser path")
		}
		return oauthproxy.LoginResult{Provider: target, Backend: "autoclaw", Path: "autoclaw-account-1234abcd.json"}, nil
	}
	t.Cleanup(func() { oauthLogin = original })

	var out bytes.Buffer
	if err := runAuth(context.Background(), &out, []string{"autoclaw"}, authOptions{}); err != nil {
		t.Fatalf("runAuth(autoclaw) error: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("OAuth login ran %d times, want 1", loginCalls)
	}
	if !strings.Contains(out.String(), "Authenticated autoclaw as provider \"autoclaw-account-1234abcd\"") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "Protocol: openai-chat / autoclaw") {
		t.Fatalf("output = %q", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers[cfg.ActiveProvider]
	if !ok || cfg.ActiveProvider != "autoclaw-account-1234abcd" {
		t.Fatalf("provider = %+v, active = %q", cfg.Providers, cfg.ActiveProvider)
	}
	// AutoClaw persists the managed Chat base and a seeded model pool instead of
	// an oauth:// runtime descriptor.
	if p.Endpoint != "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw" {
		t.Fatalf("endpoint = %q", p.Endpoint)
	}
	if p.Model != strings.Join(oauthproxy.AutoClawModelIDs(), ",") {
		t.Fatalf("model pool = %q", p.Model)
	}
	if p.CustomModelID != "zai_auto" || p.OpusModel != "zai_auto" || p.SonnetModel != "zaicoding_glm-5.3" || p.HaikuModel != "zai_glm-5.3-flash" {
		t.Fatalf("slot defaults = custom %q / opus %q / sonnet %q / haiku %q",
			p.CustomModelID, p.OpusModel, p.SonnetModel, p.HaikuModel)
	}
	if p.OAuthAccountCredential != "autoclaw-account-1234abcd.json" {
		t.Fatalf("credential binding = %q", p.OAuthAccountCredential)
	}
}
