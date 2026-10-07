package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func withClaudeSettings(t *testing.T, user, project string) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	projectDir := filepath.Join(dir, "project")
	for path, body := range map[string]string{
		filepath.Join(config, "settings.json"):                user,
		filepath.Join(projectDir, ".claude", "settings.json"): project,
	} {
		if body == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	previous := userSettingsCWD
	userSettingsCWD = func() string { return projectDir }
	t.Cleanup(func() { userSettingsCWD = previous })
}

// TestSettingsRespectTheUsersOwnPreferences pins S6: --settings outranks the
// user's settings, so ccl must not supply a preference the user already chose.
func TestSettingsRespectTheUsersOwnPreferences(t *testing.T) {
	p := provider.Provider{Type: "openai", Model: "a,b", OpusModel: "a"}

	withClaudeSettings(t, "", "")
	defaults := (&providerContext{provider: p}).settings()
	if defaults.OutputStyle == "" || defaults.Language == "" || defaults.StatusLine == nil || defaults.ModelPicker == nil {
		t.Fatalf("defaults missing with no user settings: %+v", defaults)
	}

	withClaudeSettings(t,
		`{"outputStyle":"Explanatory","language":"日本語","statusLine":{"type":"command","command":"mine"}}`,
		`{"modelPicker":{"options":[{"model":"x"}]}}`)
	respected := (&providerContext{provider: p}).settings()
	if respected.OutputStyle != "" || respected.Language != "" || respected.StatusLine != nil {
		t.Fatalf("ccl overrode user preferences: %+v", respected)
	}
	// modelPicker in project settings is ignored by Claude Code, but a user
	// who wrote one anywhere has opted into managing the lineup.
	if respected.ModelPicker != nil {
		t.Fatal("ccl replaced a lineup the user defines")
	}

	// A null value is "unset", not a choice.
	withClaudeSettings(t, `{"language":null}`, "")
	if got := (&providerContext{provider: p}).settings(); got.Language == "" {
		t.Fatal("a null user value suppressed ccl's language")
	}
}

func TestSettingsFastEffortAndUltracode(t *testing.T) {
	withClaudeSettings(t, "", "")
	off := (&providerContext{provider: provider.Provider{Type: "openai", Model: "a"}}).settings()
	if off.FastMode || off.EffortLevel != "" || off.Ultracode {
		t.Fatalf("unset provider options leaked: %+v", off)
	}
	on := (&providerContext{provider: provider.Provider{
		Type: "openai", Model: "a", FastMode: true, EffortLevel: "xhigh", Ultracode: true,
	}}).settings()
	if !on.FastMode || on.EffortLevel != "xhigh" || !on.Ultracode {
		t.Fatalf("provider options missing: %+v", on)
	}
	if _, pinned := on.Env["CLAUDE_CODE_EFFORT_LEVEL"]; pinned {
		t.Fatal("effort is still pinned through the environment")
	}
}

func TestProviderModelPickerListsTiersCustomAndPool(t *testing.T) {
	p := provider.Provider{
		Model:         "m-pro,m-flash[1m],m-chat",
		OpusModel:     "m-pro",
		HaikuModel:    "m-flash",
		CustomModelID: "m-chat",
	}
	picker := providerModelPicker(p, nil)
	if picker == nil || !picker.ReplaceBuiltInOptions {
		t.Fatalf("picker = %+v", picker)
	}
	var models []string
	for _, option := range picker.Options {
		models = append(models, option.Model)
	}
	got := strings.Join(models, ",")
	if got != "opus,haiku,m-chat,m-pro,m-flash[1m]" {
		t.Fatalf("picker rows = %s", got)
	}
	if !strings.Contains(picker.Options[0].Label, "m-pro") {
		t.Fatalf("tier row does not name its model: %+v", picker.Options[0])
	}
	if providerModelPicker(provider.Provider{}, nil) != nil {
		t.Fatal("an empty provider produced a lineup")
	}

	var many []string
	for i := range maxPickerModels + 20 {
		many = append(many, "model-"+string(rune('a'+i%26))+strings.Repeat("x", i))
	}
	if got := len(providerModelPicker(provider.Provider{Model: strings.Join(many, ",")}, nil).Options); got != maxPickerModels {
		t.Fatalf("picker rows = %d, want the cap %d", got, maxPickerModels)
	}
}
