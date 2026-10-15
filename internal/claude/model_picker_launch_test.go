package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

type modelPickerChildResult struct {
	Args     []string          `json:"args"`
	Settings settingsJSON      `json:"settings"`
	Ambient  map[string]string `json:"ambient"`
}

// modelPickerChild runs only in a copied test executable, standing in for
// Claude Code. No real Claude process, credentials or upstream are involved.
func modelPickerChild() int {
	args := os.Args[1:]
	if len(args) < 2 || args[0] != "--settings" {
		fmt.Fprintln(os.Stderr, "missing --settings argument")
		return 2
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	result := modelPickerChildResult{Args: args[2:], Ambient: make(map[string]string)}
	if err := json.Unmarshal(data, &result.Settings); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"} {
		result.Ambient[key] = os.Getenv(key)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return 2
	}
	return 0
}

func TestModelPickerReachesLaunchedProcess(t *testing.T) {
	withClaudeSettings(t, "", "")
	dir := filepath.Join(t.TempDir(), "Claude install \u6d4b\u8bd5")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	helper := filepath.Join(dir, name)
	if err := os.WriteFile(helper, data, 0o700); err != nil {
		t.Fatal(err)
	}
	oldRunDirectory := runDirectory
	runDirectory = func() (string, error) { return filepath.Join(dir, "user settings \u914d\u7f6e"), nil }
	t.Cleanup(func() { runDirectory = oldRunDirectory })
	t.Setenv("CCL_TEST_MODEL_PICKER_CHILD", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Windows environment names are case-insensitive. Stale shell overrides
	// must not rename a tier or select a different model in the child.
	for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"} {
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		t.Setenv(key, "stale-shell-value")
	}
	p := provider.Provider{
		Name: "model-picker-test", Type: "anthropic", Endpoint: "https://unused.invalid", APIKey: "fake-key",
		Model:     "acme/pro,acme/flash,acme/custom,acme/unmapped-\u6a21\u578b",
		OpusModel: "acme/pro[1m]", SonnetModel: "acme/pro", HaikuModel: "acme/flash", FableModel: "acme/pro",
		CustomModelID: "acme/custom",
	}
	types := []string{"native"}
	if runtime.GOOS == "windows" {
		types = append(types, "npm-cmd")
	}
	for _, kind := range types {
		t.Run(kind, func(t *testing.T) {
			launch, err := Prepare(p)
			if err != nil {
				t.Fatal(err)
			}
			defer launch.Close()
			if kind == "npm-cmd" {
				shim := filepath.Join(dir, "claude.cmd")
				if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0claude.exe\" %*\r\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				launch.Path = shim
			}
			// Arguments with spaces and non-ASCII must survive the launch too.
			args := []string{"--model", "acme/custom", "--resume", "session with spaces \u6d4b\u8bd5",
				"--mcp-config", `{"mcpServers":{}}`, "literal & | < > ^ ! %CCL_TEST_MODEL_PICKER_CHILD%", "", `trailing slash\`}
			output, err := launch.Command(args...).CombinedOutput()
			if err != nil {
				t.Fatalf("launch model picker reader: %v\n%s", err, output)
			}
			var got modelPickerChildResult
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatalf("child output: %v\n%s", err, output)
			}
			if !reflect.DeepEqual(got.Args, args) {
				t.Fatalf("child args = %q, want %q", got.Args, args)
			}
			for key, value := range got.Ambient {
				if value != "" {
					t.Errorf("stale child %s = %q", key, value)
				}
			}
			if got.Settings.Model != p.CustomModelID || got.Settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] != "acme/pro (1M)" {
				t.Fatalf("wrong default model or tier name: %+v", got.Settings)
			}
			picker := got.Settings.ModelPicker
			if picker == nil || !picker.ReplaceBuiltInOptions {
				t.Fatal("child did not receive provider model picker")
			}
			want := []modelPickerOption{
				{Model: "opus", Label: "Opus \u00b7 acme/pro (1M)", Description: "Opus tier \u2192 acme/pro[1m]"},
				{Model: "sonnet", Label: "Sonnet \u00b7 acme/pro", Description: "Sonnet tier \u2192 acme/pro"},
				{Model: "haiku", Label: "Haiku \u00b7 acme/flash", Description: "Haiku tier \u2192 acme/flash"},
				// Fable shares Sonnet's model, and the pool's acme/pro and
				// acme/flash are reachable from the tier rows: listed once.
				{Model: "acme/custom", Label: "Custom \u00b7 acme/custom", Description: "Default model for this provider"},
				{Model: "acme/unmapped-\u6a21\u578b", Label: "acme/unmapped-\u6a21\u578b"},
			}
			if !reflect.DeepEqual(picker.Options, want) {
				t.Fatalf("child model picker = %+v, want %+v", picker.Options, want)
			}
		})
	}
}

func TestProviderModelPickerCatalogNames(t *testing.T) {
	p := provider.Provider{
		Model:     "technical-pro,technical-flash,technical-custom,technical-other",
		OpusModel: "technical-pro[1m]", SonnetModel: "technical-pro", CustomModelID: "technical-custom",
	}
	names := map[string]string{
		"technical-pro": "Provider Pro", "technical-flash": "Provider Flash",
		"technical-custom": "Provider Custom", "technical-other": "Provider \u6a21\u578b",
	}
	picker := providerModelPicker(p, names)
	want := []modelPickerOption{
		{Model: "opus", Label: "Opus \u00b7 Provider Pro (1M)", Description: "Opus tier \u2192 Provider Pro[1m]"},
		{Model: "sonnet", Label: "Sonnet \u00b7 Provider Pro", Description: "Sonnet tier \u2192 Provider Pro"},
		{Model: "Provider Custom", Label: "Custom \u00b7 Provider Custom", Description: "Default model for this provider"},
		{Model: "Provider Flash", Label: "Provider Flash"},
		{Model: "Provider \u6a21\u578b", Label: "Provider \u6a21\u578b"},
	}
	if !reflect.DeepEqual(picker.Options, want) {
		t.Fatalf("catalog model picker = %+v, want %+v", picker.Options, want)
	}
}
