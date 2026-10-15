package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func seedStandaloneOverlay(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	fields := map[string]json.RawMessage{
		"env":            json.RawMessage(`{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345","ANTHROPIC_AUTH_TOKEN":"local-proxy-token","ANTHROPIC_API_KEY":""}`),
		"model":          json.RawMessage(`"proxy-only-model"`),
		"modelPicker":    json.RawMessage(`{"options":[{"model":"proxy-only-model"}],"replaceBuiltInOptions":true}`),
		"modelOverrides": json.RawMessage(`{}`),
		"fastMode":       json.RawMessage(`true`),
		"outputStyle":    json.RawMessage(`"Concise"`),
		"language":       json.RawMessage(`"English"`),
		"statusLine":     json.RawMessage(`{"type":"command","command":"ccl statusline"}`),
	}
	changes := []standaloneChange{
		{Env: true, Key: "ANTHROPIC_BASE_URL", Applied: json.RawMessage(`"http://127.0.0.1:12345"`)},
		{Env: true, Key: "ANTHROPIC_AUTH_TOKEN", Applied: json.RawMessage(`"local-proxy-token"`)},
		{Env: true, Key: "ANTHROPIC_API_KEY", Applied: json.RawMessage(`""`)},
		{Key: "model", Applied: fields["model"]}, {Key: "modelPicker", Applied: fields["modelPicker"]},
	}
	// Installed by the daemon while the key was absent from the user's own
	// settings, so the journal has no before value.
	for _, key := range []string{"modelOverrides", "fastMode", "outputStyle", "language", "statusLine"} {
		changes = append(changes, standaloneChange{Key: key, Applied: fields[key]})
	}
	for path, value := range map[string]any{
		UserSettingsPath(): fields,
		filepath.Join(home, ".ccl", "proxy", "state.json"): map[string]any{"settings_path": UserSettingsPath(), "changes": changes},
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(value)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Claude Code rejects the whole --settings file when any of these keys is null.
func assertNoRejectedNulls(t *testing.T, fields map[string]json.RawMessage, data []byte) {
	t.Helper()
	for key := range standaloneAbsentValues {
		if string(fields[key]) == "null" {
			t.Fatalf("%s must never marshal as null: %s", key, data)
		}
	}
}

func TestStandaloneOverlayDoesNotChangeOtherCCLProviders(t *testing.T) {
	for _, auth := range []string{"", "bearer"} {
		t.Run(auth, func(t *testing.T) {
			seedStandaloneOverlay(t)
			p := provider.Provider{Name: "another", Type: "anthropic", Endpoint: "https://another.invalid", APIKey: "another-key", AnthropicAuth: auth, Model: "another-model"}
			c := &providerContext{provider: p, baseURL: p.Endpoint}
			settings := c.settings()
			if settings.Env["ANTHROPIC_BASE_URL"] != p.Endpoint {
				t.Fatal("standalone endpoint leaked into ccl")
			}
			if auth == "bearer" {
				if settings.Env["ANTHROPIC_AUTH_TOKEN"] != p.APIKey || settings.Env["ANTHROPIC_API_KEY"] != "" {
					t.Fatal("incorrect bearer credentials")
				}
			} else if settings.Env["ANTHROPIC_API_KEY"] != p.APIKey || settings.Env["ANTHROPIC_AUTH_TOKEN"] != "" {
				t.Fatal("standalone bearer token leaked into direct API provider")
			}
			data, _ := json.Marshal(settings)
			if strings.Contains(string(data), "proxy-only-model") || settings.ModelPicker == nil {
				t.Fatalf("standalone model picker leaked: %s", data)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(data, &fields)
			if string(fields["model"]) != `""` || string(fields["modelOverrides"]) != "{}" || string(fields["fastMode"]) != "false" {
				t.Fatalf("standalone defaults were not cleared: %s", data)
			}
			assertNoRejectedNulls(t, fields, data)
		})
	}
}

func TestProviderOffCanBypassStandaloneOverlayWithoutStoppingIt(t *testing.T) {
	seedStandaloneOverlay(t)
	// The fallback for a missing before value reads the current process env;
	// pin the variables a developer's shell may carry so the test is hermetic.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "shell-original-token")
	path, cleanup, err := PlainSettingsWithoutProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	var env map[string]string
	_ = json.Unmarshal(fields["env"], &env)
	if env["ANTHROPIC_BASE_URL"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "shell-original-token" || string(fields["modelPicker"]) != "null" {
		t.Fatalf("incorrect plain settings: %s", data)
	}
	for key, want := range map[string]string{
		"model": `""`, "modelOverrides": `{}`, "fastMode": `false`, "outputStyle": `"default"`,
		"language": `""`, "statusLine": `{"type":"command","command":""}`,
	} {
		if string(fields[key]) != want {
			t.Fatalf("%s = %s, want %s: %s", key, fields[key], want, data)
		}
	}
	if len(standaloneChanges()) == 0 {
		t.Fatal("standalone overlay was removed globally")
	}
}

func TestProviderOffPreservesProjectSettingsWithStandaloneProxy(t *testing.T) {
	seedStandaloneOverlay(t)
	project := t.TempDir()
	previous := userSettingsCWD
	userSettingsCWD = func() string { return project }
	t.Cleanup(func() { userSettingsCWD = previous })
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"), []byte(`{"model":"project-model","env":{"ANTHROPIC_AUTH_TOKEN":"project-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := PlainSettingsWithoutProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, _ := os.ReadFile(path)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	var env map[string]string
	_ = json.Unmarshal(fields["env"], &env)
	if _, set := fields["model"]; set {
		t.Fatal("project model would be overridden")
	}
	if _, set := env["ANTHROPIC_AUTH_TOKEN"]; set {
		t.Fatal("project auth token would be overridden")
	}
}
