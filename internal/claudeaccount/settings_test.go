package claudeaccount

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAccountSettingsRejectAuthenticationConflicts(t *testing.T) {
	for _, fixture := range []struct{ name, body, key string }{
		{"gateway", `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345"}}`, "ANTHROPIC_BASE_URL"},
		{"helper", `{"apiKeyHelper":"echo secret-fixture"}`, "apiKeyHelper"},
		{"token", `{"env":{"ANTHROPIC_AUTH_TOKEN":"secret-fixture"}}`, "ANTHROPIC_AUTH_TOKEN"},
		{"profile", `{"env":{"CLAUDE_CONFIG_DIR":"secret-fixture"}}`, "CLAUDE_CONFIG_DIR"},
		{"platform", `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`, "CLAUDE_CODE_USE_BEDROCK"},
		{"case", `{"env":{"anthropic_api_key":"secret-fixture"}}`, "anthropic_api_key"},
		{"login-method", `{"forceLoginMethod":"console"}`, "forceLoginMethod"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(path, []byte(fixture.body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := CheckSettings(dir, "")
			if err == nil || !strings.Contains(err.Error(), fixture.key) || !strings.Contains(err.Error(), strconv.Quote(path)) || strings.Contains(err.Error(), "secret-fixture") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}

func TestAccountSettingsSourcesAndNativePreferences(t *testing.T) {
	profile, project := t.TempDir(), t.TempDir()
	settings := `{"language":"English","effortLevel":"high","permissions":{"allow":[]},"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com/","CLAUDE_CODE_MAX_RETRIES":"0","CLAUDE_CODE_USE_BEDROCK":"0"}}`
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSettings(profile, project); err != nil {
		t.Fatalf("native preferences rejected: %v", err)
	}
	for _, path := range []string{filepath.Join(project, ".claude", "settings.local.json"), filepath.Join(profile, "managed-settings.json")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"apiKeyHelper":"echo fake"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckSettings(profile, filepath.Join(project, "subdir")); err == nil || !strings.Contains(err.Error(), strconv.Quote(path)) {
			t.Fatalf("missed settings source %q: %v", path, err)
		}
		_ = os.Remove(path)
	}
}

func TestAccountExtraSettingsAreCheckedWithoutReplacingPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	if err := os.WriteFile(path, []byte(`{"effortLevel":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckArgs([]string{"--settings", path, "-p", "hello"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--settings", `{"apiKeyHelper":"echo secret-fixture"}`},
		{`--settings={"env":{"ANTHROPIC_BASE_URL":"https://wrong.invalid"}}`},
		{"--append-system-prompt", "--", "--settings", `{"apiKeyHelper":"echo secret-fixture"}`},
		{"--system-prompt", "--", `--settings={"env":{"ANTHROPIC_BASE_URL":"https://wrong.invalid"}}`},
	} {
		if err := CheckArgs(args); err == nil || strings.Contains(err.Error(), "secret-fixture") {
			t.Fatalf("unsafe settings accepted or exposed: %v", err)
		}
	}
	if err := CheckArgs([]string{"--", "--settings"}); err != nil {
		t.Fatal("prompt after -- was treated as settings")
	}
	if err := CheckArgs([]string{"--system-prompt", "--settings", "auth", "status"}); err != nil {
		t.Fatal("system prompt value was treated as an option")
	}
}
