package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
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
	return testServer(t, Config{Token: "secret"})
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

func TestConfigRejectsUnsupportedTypesAndClearsLegacyDefault(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = provider.DefaultProvider
		cfg.LegacyClaudeDefault = "retired-profile"
	})
	server := newConfigServer(t)
	for _, badType := range []string{"anthropic", "zed"} {
		status, response := doJSON(t, "POST", server.URL+"/api/config/providers", "secret",
			`{"name":"bad","type":"`+badType+`","endpoint":"https://wrong.invalid","apiKey":"fake-must-not-appear"}`)
		if status != http.StatusBadRequest || strings.Contains(string(response), "fake-must-not-appear") {
			t.Fatalf("unsupported provider type %q was not rejected safely: %d %s", badType, status, response)
		}
	}
	cfg, err := config.Load()
	if err != nil || len(cfg.Providers) != 0 {
		t.Fatalf("rejected provider was saved: %v %+v", err, cfg)
	}
	// Explicitly selecting default in Settings clears LegacyClaudeDefault, just like `ccl use default`.
	if status, body := doJSON(t, "PUT", server.URL+"/api/config", "secret", `{"active":{"normal":"default"}}`); status != http.StatusOK {
		t.Fatalf("PUT default = %d: %s", status, body)
	}
	if cfg, err = config.Load(); err != nil || cfg.LegacyClaudeDefault != "" {
		t.Fatalf("LegacyClaudeDefault after PUT default = %q (%v)", cfg.LegacyClaudeDefault, err)
	}
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
	if status, body := doJSON(t, "PATCH", url, "secret", `{"apiKey":"","models":"model-new"}`); status != http.StatusOK {
		t.Fatalf("PATCH empty key = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["work"].APIKey; got != "sk-original-0001" {
		t.Fatalf("apiKey after \"\" = %q, want the stored key", got)
	}
	if got := cfg.Providers["work"].Model; got != "model-new" {
		t.Fatalf("model = %q", got)
	}

	// An absent apiKey also leaves it alone.
	if status, body := doJSON(t, "PATCH", url, "secret", `{"effortLevel":"high"}`); status != http.StatusOK {
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
		`{"name":"newbie","type":"openai","endpoint":"https://new.example/v1","apiKey":"sk-new-1234","models":"model-s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST provider = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["newbie"].Endpoint != "https://new.example/v1" || cfg.Providers["newbie"].APIKey != "sk-new-1234" {
		t.Fatalf("created provider = %+v", cfg.Providers["newbie"])
	}

	if status, body := doJSON(t, "DELETE", httpServer.URL+"/api/config/providers/newbie", "secret", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}
	if cfg, _ := config.Load(); len(cfg.Providers) != 1 {
		t.Fatalf("providers after delete = %v", cfg.Providers)
	}

	// The reserved selector is not editable.
	if status, _ := doJSON(t, "PATCH", httpServer.URL+"/api/config/providers/default", "secret", `{"models":"x"}`); status != http.StatusForbidden {
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

func TestConfigPutGlobalSettings(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = provider.DefaultProvider
		cfg.Providers["a"] = provider.Provider{Name: "a", Type: "openai", Endpoint: "https://a.example/v1"}
		cfg.Providers["b"] = provider.Provider{Name: "b", Type: "openai", Endpoint: "https://b.example/v1"}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "PUT", httpServer.URL+"/api/config", "secret",
		`{"active":{"normal":"b"},"lang":"zh","bypassMode":true}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /api/config = %d: %s", status, body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "b" || cfg.Lang != "zh" || !cfg.BypassMode {
		t.Fatalf("config after PUT = active %q lang %q bypass %v",
			cfg.ActiveProvider, cfg.Lang, cfg.BypassMode)
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
	isolateConfig(t)
	p := provider.Provider{
		Model: "pool-a[1m], mid-model ,vendor/custom-1, pool-a",
	}
	got := providerView(&provider.Config{}, "p", p).ModelChoices
	want := []ModelChoice{
		{Value: "pool-a", Label: "pool-a", SupportsEffort: true, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "low"},
		{Value: "mid-model", Label: "mid-model", SupportsEffort: true, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "low"},
		{Value: "vendor/custom-1", Label: "vendor/custom-1", SupportsEffort: true, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "low"},
	}
	if len(got) != len(want) {
		t.Fatalf("choices = %+v", got)
	}
	for i := range want {
		if got[i].Value != want[i].Value || got[i].Label != want[i].Label || got[i].SupportsEffort != want[i].SupportsEffort || !slices.Equal(got[i].EffortLevels, want[i].EffortLevels) || got[i].DefaultEffort != want[i].DefaultEffort {
			t.Fatalf("choices[%d] = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
	}
	// An empty provider still sends a list, never null: the picker iterates it.
	if empty := providerView(&provider.Config{}, "e", provider.Provider{}).ModelChoices; empty == nil {
		t.Fatal("ModelChoices is nil for a provider without models")
	}
}

func TestModelChoicesWithModelsDev(t *testing.T) {
	home := isolateConfig(t)
	cachePath, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes, err := os.ReadFile("../modelsdev/modelsdev.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, catalogBytes, 0600); err != nil {
		t.Fatal(err)
	}
	_ = home

	// Test WorkBuddy provider with deepseek-v4.1-flash, space-bunny, and gpt-4o
	wb := provider.Provider{
		Name:          "wb",
		OAuthProvider: "workbuddy",
		Model:         "deepseek-v4.1-flash, space-bunny, gpt-4o, unknown-custom",
		EffortLevel:   "max",
	}
	choices := modelChoices(wb)
	if len(choices) != 4 {
		t.Fatalf("got %d choices, want 4", len(choices))
	}

	// deepseek-v4.1-flash: low, high, max
	ds := choices[0]
	if ds.Value != "deepseek-v4.1-flash" || !ds.SupportsEffort {
		t.Fatalf("deepseek-v4.1-flash: %+v", ds)
	}
	if !slices.Equal(ds.EffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("deepseek-v4.1-flash levels = %v, want [low high max]", ds.EffortLevels)
	}
	if ds.DefaultEffort != "max" {
		t.Fatalf("deepseek-v4.1-flash default effort = %q, want max", ds.DefaultEffort)
	}

	// space-bunny: low, medium, high, xhigh, max
	sb := choices[1]
	if sb.Value != "space-bunny" || !sb.SupportsEffort {
		t.Fatalf("space-bunny: %+v", sb)
	}
	if !slices.Equal(sb.EffortLevels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("space-bunny levels = %v, want [low medium high xhigh max]", sb.EffortLevels)
	}
	if sb.DefaultEffort != "max" {
		t.Fatalf("space-bunny default effort = %q, want max", sb.DefaultEffort)
	}

	// gpt-4o: reasoning: false -> SupportsEffort: false
	gpt4o := choices[2]
	if gpt4o.Value != "gpt-4o" || gpt4o.SupportsEffort || len(gpt4o.EffortLevels) != 0 {
		t.Fatalf("gpt-4o: %+v, want SupportsEffort=false and empty levels", gpt4o)
	}

	// unknown-custom: follows loopback_front GPT reasoning fallback (excluding ultra for Grok Build)
	custom := choices[3]
	if custom.Value != "unknown-custom" || !custom.SupportsEffort {
		t.Fatalf("unknown-custom: %+v", custom)
	}
	if !slices.Equal(custom.EffortLevels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("unknown-custom levels = %v, want [low medium high xhigh max]", custom.EffortLevels)
	}

	// Kiro OAuth maps onto anthropic in models.dev and clamps ultra EffortLevel onto max.
	kiro := provider.Provider{
		Name:          "kiro-p",
		OAuthProvider: "kiro",
		Model:         "claude-opus-4-6",
		EffortLevel:   "ultra",
	}
	kiroChoices := modelChoices(kiro)
	if len(kiroChoices) != 1 || !kiroChoices[0].SupportsEffort || !slices.Contains(kiroChoices[0].EffortLevels, "max") || kiroChoices[0].DefaultEffort != "max" {
		t.Fatalf("kiro choices = %+v, want anthropic effort ladder with max default", kiroChoices)
	}

	customMixed := provider.Provider{
		Name:  "custom-mixed",
		Model: "claude-opus-4-6-thinking, gemini-4-argon, gemini-4-argon-low, kimi-for-coding, kimi-for-coding-highspeed",
		ModelLabels: map[string]string{
			"claude-opus-4-6-thinking": "claude-opus-4-6",
		},
	}
	byValue := make(map[string]ModelChoice)
	for _, c := range modelChoices(customMixed) {
		byValue[c.Value] = c
	}
	if c := byValue["claude-opus-4-6-thinking"]; !c.SupportsEffort || !slices.Equal(c.EffortLevels, []string{"low", "medium", "high", "max"}) {
		t.Fatalf("claude-opus-4-6-thinking = %+v, want [low medium high max]", c)
	}
	if c := byValue["gemini-4-argon"]; !c.SupportsEffort || !slices.Equal(c.EffortLevels, []string{"low", "medium", "high"}) {
		t.Fatalf("gemini-4-argon = %+v, want [low medium high]", c)
	}
	if c := byValue["gemini-4-argon-low"]; c.SupportsEffort || len(c.EffortLevels) != 0 {
		t.Fatalf("gemini-4-argon-low = %+v, want SupportsEffort=false", c)
	}
	if c := byValue["kimi-for-coding"]; !c.SupportsEffort || !slices.Equal(c.EffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("kimi-for-coding = %+v, want [low high max]", c)
	}
	if c := byValue["kimi-for-coding-highspeed"]; c.SupportsEffort || len(c.EffortLevels) != 0 {
		t.Fatalf("kimi-for-coding-highspeed = %+v, want SupportsEffort=false", c)
	}

	// Verify offline fallback without modelsdev cache for kimi-for-coding & gemini-*.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("USERPROFILE", emptyHome)
	if c := modelChoices(provider.Provider{Name: "kimi-offline", Model: "kimi-for-coding"})[0]; !c.SupportsEffort || !slices.Equal(c.EffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("offline kimi-for-coding = %+v, want [low high max]", c)
	}
}

func TestValidateEffortLevels(t *testing.T) {
	for _, valid := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra", "LOW", "Max", ""} {
		if err := validateEffort(valid); err != nil {
			t.Errorf("validateEffort(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{"super", "extreme", "none", "off"} {
		if err := validateEffort(invalid); err == nil {
			t.Errorf("validateEffort(%q) should have failed", invalid)
		}
	}
}

func TestConfigProviderDefaultModelPatchAndClear(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "alpha"
		cfg.Providers["alpha"] = provider.Provider{
			Name:     "alpha",
			Type:     "openai",
			Endpoint: "https://alpha.example/v1",
			APIKey:   "sk-alpha",
			Model:    "model-a,model-b",
		}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "PATCH", httpServer.URL+"/api/config/providers/alpha", "secret", `{"defaultModel":" model-b "}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH set defaultModel = %d: %s", status, body)
	}
	var cfgView ConfigView
	if err := json.Unmarshal(body, &cfgView); err != nil {
		t.Fatal(err)
	}
	if got := cfgView.Providers["alpha"].DefaultModel; got != "model-b" {
		t.Fatalf("PATCH providers[alpha].DefaultModel = %q, want model-b", got)
	}

	status, body = doJSON(t, "PATCH", httpServer.URL+"/api/config/providers/alpha", "secret", `{"defaultModel":null}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH clear defaultModel = %d: %s", status, body)
	}
	cfgView = ConfigView{}
	if err := json.Unmarshal(body, &cfgView); err != nil {
		t.Fatal(err)
	}
	if got := cfgView.Providers["alpha"].DefaultModel; got != "" {
		t.Fatalf("PATCH cleared providers[alpha].DefaultModel = %q, want empty", got)
	}
}

func TestProviderModelsEndpointReturnsUnifiedEffortMetadata(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "kimi-gw"
		cfg.Providers["kimi-gw"] = provider.Provider{
			Name:          "kimi-gw",
			Type:          "kimi",
			OAuthProvider: oauthproxy.ProviderKimi,
			Model:         "kimi-for-coding,kimi-for-coding-highspeed",
			DefaultModel:  "kimi-for-coding-highspeed",
		}
	})
	httpServer := newConfigServer(t)

	status, body := doJSON(t, "GET", httpServer.URL+"/api/providers/kimi-gw/models", "secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/providers/kimi-gw/models = %d: %s", status, body)
	}
	var payload struct {
		Models       []ModelChoice `json:"models"`
		EffortLevels []string      `json:"effortLevels"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) < 2 || payload.Models[0].Value != "kimi-for-coding-highspeed" {
		t.Fatalf("models = %+v, want kimi-for-coding-highspeed promoted first", payload.Models)
	}
	var found bool
	for _, m := range payload.Models {
		if m.Value == "kimi-for-coding" {
			found = true
			if !slices.Equal(m.EffortLevels, []string{"low", "high", "max"}) || m.DefaultEffort != "high" {
				t.Fatalf("kimi-for-coding = %+v, want [low high max] / high matching /v1/models", m)
			}
		}
	}
	if !found {
		t.Fatalf("kimi-for-coding missing from %+v", payload.Models)
	}

	status, body = doJSON(t, "GET", httpServer.URL+"/api/providers/default/models", "secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/providers/default/models = %d: %s", status, body)
	}
	payload = struct {
		Models       []ModelChoice `json:"models"`
		EffortLevels []string      `json:"effortLevels"`
	}{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(payload.EffortLevels, oauthproxy.DefaultNativeGrokEffortLevels()) {
		t.Fatalf("default effortLevels = %v, want %v", payload.EffortLevels, oauthproxy.DefaultNativeGrokEffortLevels())
	}
}

func TestCapabilitiesExposeDefaultProviderModelAndEfforts(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(cfg *provider.Config) {
		cfg.ActiveProvider = "kimi-gw"
		cfg.Providers["kimi-gw"] = provider.Provider{
			Name:          "kimi-gw",
			Type:          "kimi",
			OAuthProvider: oauthproxy.ProviderKimi,
			Model:         "kimi-for-coding,kimi-for-coding-highspeed",
			DefaultModel:  "kimi-for-coding",
		}
	})
	httpServer := testServer(t, Config{Token: "secret", DefaultEffort: "high"})

	status, body := doJSON(t, "GET", httpServer.URL+"/api/capabilities", "secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/capabilities = %d: %s", status, body)
	}
	var caps struct {
		DefaultProvider     string   `json:"defaultProvider"`
		DefaultModel        string   `json:"defaultModel"`
		DefaultEffort       string   `json:"defaultEffort"`
		DefaultEffortLevels []string `json:"defaultEffortLevels"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.DefaultProvider != "kimi-gw" || caps.DefaultModel != "kimi-for-coding" || caps.DefaultEffort != "high" {
		t.Fatalf("capabilities defaults = %+v", caps)
	}
	if !slices.Equal(caps.DefaultEffortLevels, oauthproxy.DefaultNativeGrokEffortLevels()) {
		t.Fatalf("defaultEffortLevels = %v, want %v", caps.DefaultEffortLevels, oauthproxy.DefaultNativeGrokEffortLevels())
	}
}
