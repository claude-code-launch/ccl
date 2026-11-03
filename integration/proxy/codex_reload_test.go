package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
)

func TestProxyReloadKeepsCodexEndpoint(t *testing.T) {
	binary := os.Getenv("CCL_TEST_BINARY")
	if binary == "" {
		t.Skip("set CCL_TEST_BINARY to a built native ccl binary")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": codexHome, "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "CCL_PROXY_NO_MENUBAR": "1", "CCL_LANG": "en"} {
		t.Setenv(key, value)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/native/responses":
			if r.Header.Get("Authorization") != "Bearer fake-native-key" || r.Header.Get("Chatgpt-Account-Id") != "fake-account" {
				t.Error("native credential or account changed across reload")
			}
			_, _ = io.WriteString(w, `{"route":"native-ok"}`)
		case "/v1/messages":
			if r.Header.Get("X-Api-Key") != "fake-provider-key" || r.Header.Get("Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
				t.Error("native credentials crossed the provider boundary")
			}
			_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"text","text":"converted-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.Error(w, "unexpected upstream path", 404)
		}
	}))
	t.Cleanup(upstream.Close)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("openai_base_url = '"+upstream.URL+"/native'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"models":[{"slug":"native-fixture"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{Name: "first", Type: "anthropic", Endpoint: upstream.URL + "/v1", APIKey: "fake-provider-key", Model: "model-a", CustomModelID: "model-a"}
	other := p
	other.Name = "second"
	if err := config.Save(&provider.Config{ActiveProvider: p.Name, Providers: map[string]provider.Provider{p.Name: p, other.Name: other}}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = home
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("ccl %v: %v\n%s", args, err, output)
		}
	}
	t.Cleanup(func() { run("proxy", "stop") })
	m := &proxymanager.Manager{Dir: filepath.Join(home, ".ccl", "proxy")}
	state := func() *proxymanager.State {
		t.Helper()
		s, err := m.Read()
		if err != nil || s == nil || s.Codex == nil {
			t.Fatalf("missing running Codex state: %v", err)
		}
		return s
	}
	run("proxy", "start", p.Name)
	first := state()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	check := func(providerName string) {
		t.Helper()
		current := state()
		if current.Codex.BaseURL != first.Codex.BaseURL {
			t.Fatalf("cached client endpoint lost: %s -> %s", first.Codex.BaseURL, current.Codex.BaseURL)
		}
		for model, want := range map[string]string{"native-fixture": "native-ok", "ccl/" + providerName + "/model-a": "converted-ok"} {
			// Deliberately keep the first URL, as an already-open Codex does.
			body, _ := json.Marshal(map[string]any{"model": model, "input": "fixture"})
			request, _ := http.NewRequest(http.MethodPost, first.Codex.BaseURL+"/responses", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer fake-native-key")
			request.Header.Set("Chatgpt-Account-Id", "fake-account")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !strings.Contains(string(raw), want) {
				t.Fatalf("cached endpoint request failed: %d %s", response.StatusCode, raw)
			}
		}
	}
	check(p.Name)
	// ccl set can refresh metadata without changing provider YAML. An explicit
	// proxy start must pick up that snapshot while retaining the cached port.
	metadataPath, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(metadataPath, []byte(`{"fixture":{"api":"https://example.invalid","models":{"model-a":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","max"]}]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run("proxy", "start", p.Name)
	refreshed := state()
	if refreshed.ID == first.ID || refreshed.MetadataStamp == "" {
		t.Fatal("metadata refresh did not replace the daemon")
	}
	catalogBytes, err := os.ReadFile(refreshed.Codex.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []struct {
			Slug   string `json:"slug"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(catalogBytes, &catalog); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, model := range catalog.Models {
		if model.Slug == "ccl/"+p.Name+"/model-a" {
			matched = len(model.Levels) == 2 && model.Levels[0].Effort == "low" && model.Levels[1].Effort == "max"
		}
	}
	if !matched {
		t.Fatal("refreshed effort metadata did not reach the client catalog")
	}
	check(p.Name)
	run("proxy", "start", p.Name)
	if state().ID != refreshed.ID {
		t.Fatal("unchanged metadata needlessly restarted the proxy")
	}
	legacy := *refreshed
	legacy.ProxyVersion--
	raw, _ := json.Marshal(legacy)
	if err := fsutil.WriteFileAtomic(filepath.Join(m.Dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	run("proxy", "start", p.Name)
	if state().ID == refreshed.ID {
		t.Fatal("legacy daemon was not replaced")
	}
	check(p.Name)
	beforeReload := state().ID
	if err := config.Update(func(cfg *provider.Config) error {
		updated := cfg.Providers[p.Name]
		updated.SonnetModel = "model-a"
		cfg.Providers[p.Name] = updated
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run("proxy", "start", p.Name)
	if state().ID == beforeReload {
		t.Fatal("provider configuration change did not replace the daemon")
	}
	check(p.Name)
	run("proxy", "start", other.Name)
	check(other.Name)
	run("proxy", "start", "--independent", other.Name)
	independent := state()
	if independent.CodexMode != "independent" || independent.Codex.BaseURL != first.Codex.BaseURL {
		t.Fatal("independent mode lost its state or port")
	}
	configBytes, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBytes), `model_provider = "ccl_proxy"`) || !strings.Contains(string(configBytes), "requires_openai_auth = false") {
		t.Fatal("independent provider is not active")
	}
	run("proxy", "start", other.Name)
	if state().ID != independent.ID {
		t.Fatal("implicit start did not retain independent mode")
	}
	run("proxy", "start", "--shared", other.Name)
	if state().CodexMode != "shared" {
		t.Fatal("explicit shared switch was ignored")
	}
	check(other.Name)
	configBytes, _ = os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if strings.Contains(string(configBytes), "[model_providers.ccl_proxy]") {
		t.Fatal("temporary provider credentials survived switching back")
	}
}
