package claudeaccount_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// This uses real Claude Code's read-only auth status with fake, unexpired
// credentials and an isolated home. No login or model request is made.
func TestRealClaudeAccountIsolation(t *testing.T) {
	binary, claude := os.Getenv("CCL_TEST_BINARY"), os.Getenv("CCL_TEST_CLAUDE")
	if binary == "" || claude == "" {
		t.Skip("set CCL_TEST_BINARY and CCL_TEST_CLAUDE for the real CLI account test")
	}
	home := filepath.Join(t.TempDir(), "Claude user \u6d4b\u8bd5")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, entry := range []struct{ key, path string }{
		{"APPDATA", filepath.Join(home, "AppData", "Roaming")},
		{"LOCALAPPDATA", filepath.Join(home, "AppData", "Local")},
	} {
		if err := os.MkdirAll(entry.path, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(entry.key, entry.path)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "original profile"))
	t.Setenv("ANTHROPIC_API_KEY", "stale-fake-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-fake-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://unused.invalid")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "stale-fake-oauth")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("NO_PROXY", "")
	binDir := filepath.Join(home, "Claude binary \u914d\u7f6e")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(claude)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(binDir, name), os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store, err := claudeaccount.Open()
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]provider.Provider{}
	for _, account := range []string{"work", "personal"} {
		candidate, err := store.Prepare(account)
		if err != nil {
			t.Fatal(err)
		}
		credential := map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "fake-" + account, "refreshToken": "fake-refresh-" + account,
			"expiresAt": int64(4102444800000), "scopes": []string{"user:inference"}, "subscriptionType": "max",
		}}
		profile := map[string]any{"hasCompletedOnboarding": true, "oauthAccount": map[string]any{
			"accountUuid": "fake-" + account, "emailAddress": account + "@test.invalid", "organizationUuid": "fake-org",
		}}
		for filename, value := range map[string]any{".credentials.json": credential, ".claude.json": profile} {
			data, _ := json.Marshal(value)
			if err := os.WriteFile(filepath.Join(candidate.Path, filename), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		providers[account] = provider.Provider{Name: account, Type: "anthropic", Endpoint: "oauth://claude", OAuthProvider: "claude", OAuthAccountCredential: candidate.ID()}
	}
	if err := config.Save(&provider.Config{ActiveProvider: "work", DefaultClaudeProvider: "personal", Providers: providers}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"work", "personal", "work"} {
		cmd := exec.Command(binary, "--provider", account, "auth", "status", "--json")
		cmd.Dir = home
		output, err := cmd.Output()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				t.Fatalf("%s auth status: %v: %s", account, err, exit.Stderr)
			}
			t.Fatal(err)
		}
		var result struct {
			LoggedIn        bool   `json:"loggedIn"`
			AuthMethod      string `json:"authMethod"`
			Email           string `json:"email"`
			ConfigDirectory string `json:"configDirectory"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("auth status is not JSON: %v: %s", err, output)
		}
		profile, err := store.Profile(providers[account].OAuthAccountCredential)
		if err != nil || !result.LoggedIn || result.AuthMethod != "claude.ai" || result.Email != account+"@test.invalid" || filepath.Clean(result.ConfigDirectory) != filepath.Clean(profile) {
			t.Fatalf("wrong account for %s: %+v, profile=%q, err=%v", account, result, profile, err)
		}
	}
	cfg, err := config.Load()
	if err != nil || cfg.ActiveProvider != "work" {
		t.Fatalf("one-shot account launch changed provider: %+v %v", cfg, err)
	}
	for _, step := range []struct {
		args  []string
		email string
	}{
		{[]string{"use", "default"}, "personal@test.invalid"},
		{[]string{"use", "work", "--default"}, "work@test.invalid"},
		{[]string{"use", "default"}, "work@test.invalid"},
	} {
		if output, err := exec.Command(binary, step.args...).CombinedOutput(); err != nil {
			t.Fatalf("ccl %v: %v %s", step.args, err, output)
		}
		cmd := exec.Command(binary, "claude", "auth", "status", "--json")
		cmd.Dir = home
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("auth status: %v", err)
		}
		var status struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(output, &status); err != nil || status.Email != step.email {
			t.Fatalf("ccl %v selected %s, want %s; output=%s, err=%v", step.args, status.Email, step.email, output, err)
		}
	}
}
