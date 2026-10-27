package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// isolateConfig points HOME at a temporary directory so a test can write a
// config without touching the developer's ~/.ccl.
func isolateConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func seedConfig(t *testing.T, mutate func(*provider.Config)) {
	t.Helper()
	if err := config.Update(func(cfg *provider.Config) error {
		mutate(cfg)
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func newConfigServer(t *testing.T) *httptest.Server {
	t.Helper()
	server, err := New(Config{Workspace: t.TempDir(), StoreRoot: t.TempDir(), Token: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func doJSON(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	var request *http.Request
	var err error
	if body == "" {
		request, err = http.NewRequest(method, url, nil)
	} else {
		request, err = http.NewRequest(method, url, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	payload := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	for {
		n, readErr := response.Body.Read(buffer)
		payload = append(payload, buffer[:n]...)
		if readErr != nil {
			break
		}
	}
	return response.StatusCode, payload
}

func TestConfigMasksAPIKey(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "work"
		cfg.Providers["work"] = provider.Provider{
			Name:     "work",
			Type:     "openai",
			Endpoint: "https://example.com/v1",
			APIKey:   "sk-super-secret-1234",
		}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "GET", httpServer.URL+"/api/config", "secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/config = %d: %s", status, body)
	}
	var view ConfigView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if strings.Contains(string(body), "sk-super-secret-1234") {
		t.Fatal("the full API key left the server")
	}
	work := view.Providers["work"]
	if work.APIKey != "***1234" || !work.HasKey {
		t.Fatalf("masked provider = %+v", work)
	}
	if !view.Default.IsDefault || view.Default.Name != provider.DefaultProvider {
		t.Fatalf("default view = %+v", view.Default)
	}
	if _, ok := view.Providers[provider.DefaultProvider]; ok {
		t.Fatal("default was listed as an editable provider")
	}
	if view.Active.Normal != "work" {
		t.Fatalf("active = %+v", view.Active)
	}
}

func TestConfigProviderPatchAPIKeyStates(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "work"
		cfg.Providers["work"] = provider.Provider{
			Name: "work", Type: "openai", Endpoint: "https://example.com/v1", APIKey: "sk-original-0001",
		}
	})
	httpServer := newConfigServer(t)
	url := httpServer.URL + "/api/config/providers/work"

	// "" leaves the stored key alone.
	if status, body := doJSON(t, "PATCH", url, "secret", `{"apiKey":"","opusModel":"opus-new"}`); status != http.StatusOK {
		t.Fatalf("PATCH empty key = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["work"].APIKey; got != "sk-original-0001" {
		t.Fatalf("apiKey after \"\" = %q, want the stored key", got)
	}
	if got := cfg.Providers["work"].OpusModel; got != "opus-new" {
		t.Fatalf("opusModel = %q", got)
	}

	// An absent apiKey also leaves it alone.
	if status, body := doJSON(t, "PATCH", url, "secret", `{"haikuModel":"haiku-new"}`); status != http.StatusOK {
		t.Fatalf("PATCH absent key = %d: %s", status, body)
	}
	if cfg, _ := config.Load(); cfg.Providers["work"].APIKey != "sk-original-0001" {
		t.Fatalf("apiKey after an absent field = %q", cfg.Providers["work"].APIKey)
	}

	// A value replaces it.
	if status, body := doJSON(t, "PATCH", url, "secret", `{"apiKey":"sk-replaced-9999"}`); status != http.StatusOK {
		t.Fatalf("PATCH new key = %d: %s", status, body)
	}
	if cfg, _ := config.Load(); cfg.Providers["work"].APIKey != "sk-replaced-9999" {
		t.Fatalf("apiKey after a value = %q", cfg.Providers["work"].APIKey)
	}

	// null clears it.
	if status, body := doJSON(t, "PATCH", url, "secret", `{"apiKey":null}`); status != http.StatusOK {
		t.Fatalf("PATCH null key = %d: %s", status, body)
	}
	cfg, _ = config.Load()
	if got := cfg.Providers["work"].APIKey; got != "" {
		t.Fatalf("apiKey after null = %q, want empty", got)
	}
	if strings.Contains(string(mustRead(t)), "sk-replaced-9999") {
		t.Fatal("the cleared key is still on disk")
	}
}

func mustRead(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(data)
}

func TestConfigCreateAndDeleteProvider(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = provider.DefaultProvider
		cfg.Providers["keep"] = provider.Provider{Name: "keep", Type: "openai", Endpoint: "https://keep.example/v1"}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "POST", httpServer.URL+"/api/config/providers", "secret",
		`{"name":"newbie","type":"anthropic","endpoint":"https://new.example","apiKey":"sk-new-1234","sonnetModel":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST provider = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["newbie"].Endpoint != "https://new.example" || cfg.Providers["newbie"].APIKey != "sk-new-1234" {
		t.Fatalf("created provider = %+v", cfg.Providers["newbie"])
	}

	if status, body := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/newbie", "secret", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}
	if cfg, _ := config.Load(); len(cfg.Providers) != 1 {
		t.Fatalf("providers after delete = %v", cfg.Providers)
	}

	// The reserved selector is not editable.
	if status, _ := doJSON(t, "PATCH", httpServer.URL+"/api/config/providers/default", "secret", `{"opusModel":"x"}`); status != http.StatusForbidden {
		t.Fatalf("PATCH default = %d, want 403", status)
	}
	if status, _ := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/default", "secret", ""); status != http.StatusForbidden {
		t.Fatalf("DELETE default = %d, want 403", status)
	}
	if status, _ := doJSON(t, "POST", httpServer.URL+"/api/config/providers", "secret", `{"name":"default","type":"openai","endpoint":"https://x"}`); status != http.StatusBadRequest {
		t.Fatalf("POST default = %d, want 400", status)
	}
}

func TestConfigDeleteActiveProviderConflicts(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "active"
		cfg.Providers["active"] = provider.Provider{Name: "active", Type: "openai", Endpoint: "https://a.example/v1"}
		cfg.Providers["free"] = provider.Provider{Name: "free", Type: "openai", Endpoint: "https://b.example/v1"}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/active", "secret", "")
	if status != http.StatusConflict {
		t.Fatalf("DELETE active = %d: %s", status, body)
	}
	if cfg, _ := config.Load(); len(cfg.Providers) != 2 {
		t.Fatal("a rejected delete changed the config")
	}
	if status, _ := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/free", "secret", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE inactive = %d", status)
	}
}

func TestConfigDeleteACPProviderConflicts(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "normal"
		cfg.ACPProvider = "pinned"
		cfg.Providers["normal"] = provider.Provider{Name: "normal", Type: "openai", Endpoint: "https://a.example/v1"}
		cfg.Providers["pinned"] = provider.Provider{Name: "pinned", Type: "openai", Endpoint: "https://b.example/v1"}
	})
	httpServer := newConfigServer(t)
	if status, body := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/pinned", "secret", ""); status != http.StatusConflict {
		t.Fatalf("DELETE ACP provider = %d: %s", status, body)
	}
}

func TestConfigPutGlobalSettings(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = provider.DefaultProvider
		cfg.Providers["a"] = provider.Provider{Name: "a", Type: "openai", Endpoint: "https://a.example/v1"}
		cfg.Providers["b"] = provider.Provider{Name: "b", Type: "openai", Endpoint: "https://b.example/v1"}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "PUT", httpServer.URL+"/api/config", "secret",
		`{"active":{"normal":"b","acp":"a"},"lang":"zh","bypassMode":true}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /api/config = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "b" || cfg.ACPProvider != "a" || cfg.Lang != "zh" || !cfg.BypassMode {
		t.Fatalf("config after PUT = active %q acp %q lang %q bypass %v",
			cfg.ActiveProvider, cfg.ACPProvider, cfg.Lang, cfg.BypassMode)
	}

	// An unknown provider is rejected without a write.
	if status, _ := doJSON(t, "PUT", httpServer.URL+"/api/config", "secret", `{"active":{"normal":"missing"}}`); status != http.StatusBadRequest {
		t.Fatalf("PUT unknown provider = %d, want 400", status)
	}
	if cfg, _ := config.Load(); cfg.ActiveProvider != "b" {
		t.Fatalf("a rejected PUT changed active to %q", cfg.ActiveProvider)
	}
}

func TestConfigRequiresToken(t *testing.T) {
	isolateConfig(t)
	httpServer := newConfigServer(t)
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/config", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET without a token = %d, want 401", status)
	}
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/config", "wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET with a wrong token = %d, want 401", status)
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"ab":          "**",
		"abcd":        "****",
		"sk-12345678": "***5678",
	}
	for input, want := range cases {
		if got := maskKey(input); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProviderViewMasksAndShowsType(t *testing.T) {
	p := provider.Provider{Name: "p", Type: "openai", Endpoint: "https://e", APIKey: "sk-abcd1234"}
	view := providerView(&provider.Config{ActiveProvider: "p"}, "p", p)
	if view.APIKey != "***1234" || !view.HasKey || !view.Active {
		t.Fatalf("view = %+v", view)
	}
}

func TestProviderViewContextWindow(t *testing.T) {
	for preset, want := range map[string]int{"": 0, provider.ContextPresetNameBalanced500K: 500_000, provider.ContextPresetNameBalanced800K: 800_000} {
		if got := providerView(&provider.Config{}, "p", provider.Provider{ContextPreset: preset}).ContextWindow; got != want {
			t.Fatalf("preset %q: window = %d, want %d", preset, got, want)
		}
	}
}

func TestProviderViewModelChoices(t *testing.T) {
	p := provider.Provider{
		OpusModel:     "big-model[1m]",
		SonnetModel:   "mid-model",
		CustomModelID: "vendor/custom-1",
		SubagentModel: "small-model",
		Model:         "pool-a, mid-model ,vendor/custom-1",
	}
	got := providerView(&provider.Config{}, "p", p).ModelChoices
	want := []ModelChoice{
		{Value: "opus", Label: "opus · big-model"},
		{Value: "sonnet", Label: "sonnet · mid-model"},
		{Value: "vendor/custom-1", Label: "custom · vendor/custom-1"},
		{Value: "pool-a", Label: "pool-a"},
		{Value: "mid-model", Label: "mid-model"},
	}
	if len(got) != len(want) {
		t.Fatalf("choices = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("choices[%d] = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
	}
	// An empty provider still sends a list, never null: the picker iterates it.
	if empty := providerView(&provider.Config{}, "e", provider.Provider{}).ModelChoices; empty == nil {
		t.Fatal("ModelChoices is nil for a provider without models")
	}
}
