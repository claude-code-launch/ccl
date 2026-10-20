package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// ccl signs nothing unless asked: a launch carries Claude Code's empty
// attribution texts, which hides the Co-Authored-By trailer.
func TestSettingsHideSigningByDefault(t *testing.T) {
	withClaudeSettings(t, "", "")
	previous := SigningEnabled()
	t.Cleanup(func() { ConfigureSignOn(previous) })

	ConfigureSignOn(false)
	data, err := json.Marshal((&providerContext{provider: provider.Provider{Type: "openai", Model: "a"}}).settings())
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Attribution *struct {
			Commit     string `json:"commit"`
			PR         string `json:"pr"`
			SessionURL bool   `json:"sessionUrl"`
		} `json:"attribution"`
		IncludeCoAuthoredBy *bool `json:"includeCoAuthoredBy"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Attribution == nil || fields.Attribution.Commit != "" || fields.Attribution.PR != "" || fields.Attribution.SessionURL {
		t.Fatalf("attribution = %+v; want empty texts and no session URL: %s", fields.Attribution, data)
	}
	if fields.IncludeCoAuthoredBy == nil || *fields.IncludeCoAuthoredBy {
		t.Fatalf("includeCoAuthoredBy must be false, settings: %s", data)
	}
}

func TestSettingsKeepSigningWhenOn(t *testing.T) {
	withClaudeSettings(t, "", "")
	previous := SigningEnabled()
	t.Cleanup(func() { ConfigureSignOn(previous) })

	ConfigureSignOn(true)
	data, err := json.Marshal((&providerContext{provider: provider.Provider{Type: "openai", Model: "a"}}).settings())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["attribution"]; ok {
		t.Fatalf("attribution must be left to Claude Code when signing is on: %s", data)
	}
}

func TestReadGlobalSigning(t *testing.T) {
	tests := []struct {
		name string
		body string
		want GlobalSigning
	}{
		{"absent", `{}`, GlobalSigningDefault},
		{"attribution false", `{"attribution":false}`, GlobalSigningDisabled},
		{"empty commit", `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`, GlobalSigningDisabled},
		{"deprecated switch", `{"includeCoAuthoredBy":false}`, GlobalSigningDisabled},
		{"custom text", `{"attribution":{"commit":"Co-Authored-By: me"}}`, GlobalSigningCustom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ReadGlobalSigning(); got != tt.want {
				t.Fatalf("ReadGlobalSigning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteGlobalSigningMergesAndRestores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"opus","attribution":{"commit":"Co-Authored-By: me"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Off installs ccl's attribution and keeps every other key.
	changed, err := WriteGlobalSigning(false)
	if err != nil || !changed {
		t.Fatalf("WriteGlobalSigning(false) = %v, %v", changed, err)
	}
	fields := rawSettings(path)
	if string(fields["model"]) != `"opus"` {
		t.Fatalf("other keys were lost: %v", fields)
	}
	if !sameSetting(fields["attribution"], attributionOffJSON) {
		t.Fatalf("attribution = %s", fields["attribution"])
	}
	if changed, err = WriteGlobalSigning(false); err != nil || changed {
		t.Fatalf("repeating off changed the file again: %v, %v", changed, err)
	}

	// On removes only what ccl wrote; a user's own text is not theirs to keep
	// once ccl overwrote it, so the key disappears rather than being guessed.
	if changed, err = WriteGlobalSigning(true); err != nil || !changed {
		t.Fatalf("WriteGlobalSigning(true) = %v, %v", changed, err)
	}
	fields = rawSettings(path)
	if _, ok := fields["attribution"]; ok {
		t.Fatalf("attribution ccl wrote was not removed: %v", fields)
	}
	if string(fields["model"]) != `"opus"` {
		t.Fatalf("other keys were lost: %v", fields)
	}
}

func TestWriteGlobalSigningKeepsCustomAttributionWhenOn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	custom := `{"attribution":{"commit":"Co-Authored-By: me <me@example.com>"}}`
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteGlobalSigning(true)
	if err != nil || changed {
		t.Fatalf("WriteGlobalSigning(true) = %v, %v; a user's text must be kept", changed, err)
	}
	if fields := rawSettings(path); string(fields["attribution"]) != `{"commit":"Co-Authored-By: me <me@example.com>"}` {
		t.Fatalf("user attribution was changed: %s", fields["attribution"])
	}
}
