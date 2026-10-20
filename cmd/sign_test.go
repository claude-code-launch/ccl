package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestSignTogglePersistsAndReports(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})
	if loadToggleConfig(t).SignOn {
		t.Fatal("signing must be off on a fresh config")
	}

	var out bytes.Buffer
	if err := runSign(&out, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Signing for ccl sessions = off") {
		t.Fatalf("status = %q", out.String())
	}

	out.Reset()
	if err := runSign(&out, []string{"on"}, false); err != nil {
		t.Fatal(err)
	}
	if !loadToggleConfig(t).SignOn {
		t.Fatal("ccl sign on was not persisted")
	}
	if err := runSign(&out, []string{"off"}, false); err != nil {
		t.Fatal(err)
	}
	if loadToggleConfig(t).SignOn {
		t.Fatal("ccl sign off was not persisted")
	}
}

func TestSignDefaultWritesClaudeSettings(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a", Providers: map[string]provider.Provider{
		"a": {Name: "a", Type: "openai", Endpoint: "https://example.com/v1", Model: "m"},
	}})
	userSettings := claude.UserSettingsPath()
	original := `{"model":"opus","permissions":{"allow":["Bash"]}}`
	if err := os.MkdirAll(filepath.Dir(userSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userSettings, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runSign(&out, []string{"off"}, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(userSettings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["model"]) != `"opus"` || !bytes.Contains(data, []byte(`"allow": [`)) {
		t.Fatalf("ccl sign --default lost user settings: %s", data)
	}
	var attribution struct {
		Commit string `json:"commit"`
		PR     string `json:"pr"`
	}
	if err := json.Unmarshal(fields["attribution"], &attribution); err != nil || attribution.Commit != "" || attribution.PR != "" {
		t.Fatalf("attribution = %s (%v)", fields["attribution"], err)
	}
	if got := claude.ReadGlobalSigning(); got != claude.GlobalSigningDisabled {
		t.Fatalf("ReadGlobalSigning() = %v", got)
	}

	out.Reset()
	if err := runSign(&out, []string{"on"}, true); err != nil {
		t.Fatal(err)
	}
	if got := claude.ReadGlobalSigning(); got != claude.GlobalSigningDefault {
		t.Fatalf("ReadGlobalSigning() after on = %v", got)
	}
}

func TestSignRejectsUnknownValue(t *testing.T) {
	saveToggleFixture(t, &provider.Config{ActiveProvider: "a"})
	if err := runSign(&bytes.Buffer{}, []string{"yes"}, false); err == nil {
		t.Fatal("expected an error for an unknown toggle value")
	}
}
