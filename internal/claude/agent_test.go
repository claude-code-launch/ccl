package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestAgentLaunchUsesSDKAndClientProjectPreferences(t *testing.T) {
	t.Setenv("PATH", "") // The adapter SDK supplies Claude; no CLI lookup.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "user"))
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"),
		[]byte(`{"language":"French","modelPicker":{"options":[{"model":"personal"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := runDirectory
	runDir := filepath.Join(t.TempDir(), "no-settings-files")
	runDirectory = func() (string, error) { return runDir, nil }
	t.Cleanup(func() { runDirectory = previous })
	p := provider.Provider{Name: "fixture", Type: "anthropic", Endpoint: "https://unused.invalid/v1",
		APIKey: "fake-key", Model: "model-a,model-b", CustomModelID: "model-a",
		Env: map[string]string{"CCL_AGENT_FIXTURE_CUSTOM": "provider-only"}}
	t.Setenv("CCL_AGENT_FIXTURE_CUSTOM", "original-shell")
	launch, err := PrepareAgentContext(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	data, err := launch.Options(project)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env      map[string]string `json:"env"`
		Settings settingsJSON      `json:"settings"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Settings.Model != "model-a" || got.Settings.Language != "" || got.Settings.ModelPicker != nil {
		t.Fatalf("client project preferences or model lost: %+v", got.Settings)
	}
	if got.Env["ANTHROPIC_API_KEY"] != "fake-key" || got.Env["ANTHROPIC_AUTH_TOKEN"] != "" || got.Env["CLAUDE_CODE_USE_BEDROCK"] != "0" {
		t.Fatalf("agent routing = %+v", got.Env)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("SDK preparation wrote temporary Claude settings: %v", err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-shell-token")
	t.Setenv("ANTHROPIC_API_KEY", "stale-shell-key")
	processEnv, err := AgentProcessEnv(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range processEnv {
		if strings.Contains(entry, "stale-shell-") {
			t.Fatalf("agent process inherited a stale credential: %s", entry)
		}
	}
	if !slicesContains(processEnv, "ANTHROPIC_API_KEY=fake-key") {
		t.Fatal("adapter did not receive CCL authentication")
	}
	if got.Env["CCL_AGENT_FIXTURE_CUSTOM"] != "provider-only" ||
		!slicesContains(processEnv, "CCL_AGENT_FIXTURE_CUSTOM=original-shell") ||
		slicesContains(processEnv, "CCL_AGENT_FIXTURE_CUSTOM=provider-only") {
		t.Fatal("provider custom env must belong to the SDK session, not the adapter process")
	}
}

func TestAgentLaunchKeepsProxyTransportAndAllModels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := provider.Provider{Type: "openai", Endpoint: "https://unused.invalid/v1", APIKey: "fake-upstream",
		Model: "model-a,model-b,unmapped", SonnetModel: "model-a", CustomModelID: "model-b",
		Env: map[string]string{"ANTHROPIC_BASE_URL": "https://wrong.invalid", "ANTHROPIC_AUTH_TOKEN": "wrong-token"}}
	launch, err := PrepareAgentContext(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	data, err := launch.Options(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env      map[string]string `json:"env"`
		Settings settingsJSON      `json:"settings"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Env["ANTHROPIC_BASE_URL"], "http://127.0.0.1:") ||
		!strings.HasPrefix(got.Env["ANTHROPIC_AUTH_TOKEN"], "ccl-") || got.Env["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("SDK transport is not isolated: %+v", got.Env)
	}
	found := false
	for _, option := range got.Settings.ModelPicker.Options {
		found = found || option.Model == "unmapped"
	}
	if !found || strings.Contains(string(data), "fake-upstream") || strings.Contains(string(data), "wrong-token") {
		t.Fatal("SDK model pool or credential isolation is incorrect")
	}
}

func TestAgentRuntimeOutlivesPreparationRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	launch, err := PrepareAgentContext(ctx, provider.Provider{Type: "openai", Model: "fixture", Endpoint: "https://unused.invalid/v1", APIKey: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	data, err := launch.Options(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var options struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(data, &options)
	cancel()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, options.Env["ANTHROPIC_BASE_URL"]+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+options.Env["ANTHROPIC_AUTH_TOKEN"])
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request cancellation stopped a leased runtime: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("leased runtime models status = %d", response.StatusCode)
	}
}
