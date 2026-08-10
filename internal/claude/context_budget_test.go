package claude

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestContextClassLabel(t *testing.T) {
	cases := map[int]string{
		0:         "unknown",
		128_000:   "200K default",
		272_000:   "200K default",
		500_000:   "200K default",
		900_000:   "1M-class",
		1_050_000: "1M-class",
	}
	for window, want := range cases {
		if got := ContextClassLabel(window); got != want {
			t.Errorf("ContextClassLabel(%d) = %q, want %q", window, got, want)
		}
	}
}

// codexCatalogServer serves the Codex client model catalog, the only shape that
// carries context windows for subscription runtimes.
func codexCatalogServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := request.URL.Query()["client_version"]; !ok {
			// Mirror CLIProxyAPI: without client_version the list is trimmed and
			// carries no window at all.
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"}]}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAdvertisedContextWindowsPrefersCodexCatalog(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[
		{"slug":"gpt-5.6-sol","context_window":272000},
		{"slug":"big","context_window":1050000}
	]}`)

	windows, source := AdvertisedContextWindows(server.URL, "session-key")
	if source == "" {
		t.Fatal("no catalog source reported")
	}
	if windows["gpt-5.6-sol"] != 272_000 || windows["big"] != 1_050_000 {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestMappedContextClassesDiffer(t *testing.T) {
	p := provider.Provider{OpusModel: "big[1m]", HaikuModel: "small"}
	windows := map[string]int{"big": 1_050_000, "small": 272_000}
	if !MappedContextClassesDiffer(p, windows) {
		t.Error("1M-class plus 200K-class must be reported as mixed")
	}
	if MappedContextClassesDiffer(p, map[string]int{"big": 1_050_000}) {
		t.Error("a single known window is not a mixed pool")
	}
	if MappedContextClassesDiffer(p, nil) {
		t.Error("no catalog means nothing to compare")
	}
}

func TestApplyContextPolicyDropsUnsupportedOverrides(t *testing.T) {
	overrides := []map[string]string{
		{provider.EnvMaxContextTokens: "1000000", provider.EnvAutoCompactWindow: "900000"},
		{provider.EnvMaxContextTokens: "500000", provider.EnvAutoCompactWindow: "400000"},
		{provider.EnvMaxContextTokens: "300000", provider.EnvAutoCompactWindow: "200000"},
		{provider.EnvAutoCompactWindow: "1000000", provider.EnvAutoCompactPct: "90"},
		{provider.EnvAutoCompactWindow: "1000000"},
		{provider.EnvMaxContextTokens: "1050000", provider.EnvAutoCompactWindow: "840000"},
	}
	for _, override := range overrides {
		env := map[string]string{"KEEP": "yes"}
		for key, value := range override {
			env[key] = value
		}
		if !applyContextPolicy(env) {
			t.Fatalf("override %#v was not removed", override)
		}
		for _, key := range provider.ManagedContextEnvKeys() {
			if _, present := env[key]; present {
				t.Errorf("override %#v left %s behind", override, key)
			}
		}
		if env["KEEP"] != "yes" {
			t.Errorf("override %#v removed unrelated env", override)
		}
	}
}

func TestApplyContextPolicyKeepsBalanced(t *testing.T) {
	env := map[string]string{
		provider.EnvMaxContextTokens:  "500000",
		provider.EnvAutoCompactWindow: "500000",
		provider.EnvAutoCompactPct:    "80",
	}
	if applyContextPolicy(env) {
		t.Fatal("Balanced must survive the launcher policy")
	}
	if !provider.IsBalancedContextPreset(env) {
		t.Fatalf("Balanced values were modified: %#v", env)
	}
}

func TestSettingsDoNotDeclareContextByDefault(t *testing.T) {
	// A provider carrying an old ccl preset must produce a session without any
	// context env, so Claude Code sizes each slot itself.
	ctx := &providerContext{
		provider: provider.Provider{
			Name:      "gpt-oauth",
			Type:      "openai_responses",
			APIKey:    "session-key",
			OpusModel: "gpt-5.6-sol[1m]",
			Env: map[string]string{
				provider.EnvMaxContextTokens:  "1000000",
				provider.EnvAutoCompactWindow: "900000",
			},
		},
		baseURL: "http://127.0.0.1:1234",
	}
	settings := ctx.settings()
	for _, key := range provider.ManagedContextEnvKeys() {
		if value, present := settings.Env[key]; present {
			t.Errorf("%s = %q, want it absent from the settings file", key, value)
		}
	}
	if !ctx.droppedContextOverride {
		t.Error("dropping the preset was not recorded for the log")
	}
	// The [1m] marker is the sizing signal and must survive.
	if settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "gpt-5.6-sol[1m]" {
		t.Errorf("opus model = %q, want the [1m] marker preserved", settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
}

func TestSettingsKeepBalancedContextTriplet(t *testing.T) {
	ctx := &providerContext{
		provider: provider.Provider{
			Name:   "balanced",
			Type:   "anthropic",
			APIKey: "test-key",
			Env: map[string]string{
				provider.EnvMaxContextTokens:  "500000",
				provider.EnvAutoCompactWindow: "500000",
				provider.EnvAutoCompactPct:    "80",
			},
		},
		baseURL: "https://example.test",
	}
	settings := ctx.settings()
	if !provider.IsBalancedContextPreset(settings.Env) {
		t.Fatalf("Balanced settings were not retained: %#v", settings.Env)
	}
	if ctx.droppedContextOverride {
		t.Fatal("Balanced was incorrectly treated as an unsupported preset")
	}
}

func TestBuildEnvDropsCclDirectives(t *testing.T) {
	p := provider.Provider{
		Name:     "manual-gpt",
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		APIKey:   "sk-test",
		Env: map[string]string{
			provider.EnvContextBudgetMode: "manual",
			provider.EnvMaxContextTokens:  "1050000",
		},
	}
	env := buildEnv(p, "https://example.test", false)
	if _, present := env[provider.EnvContextBudgetMode]; present {
		t.Errorf("%s is a ccl directive and must not reach Claude Code", provider.EnvContextBudgetMode)
	}
	if env[provider.EnvMaxContextTokens] != "1050000" {
		t.Errorf("configured context override was lost: %q", env[provider.EnvMaxContextTokens])
	}
}

func TestBuildProcessEnvDropsInheritedSettingsKeys(t *testing.T) {
	// Default writes no context env, so ambient values must not silently replace it.
	settings := settingsJSON{Env: map[string]string{
		"CUSTOM_SETTING":    "from-ccl",
		"ANTHROPIC_API_KEY": "sk-from-ccl",
	}}
	inherited := []string{
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://corp:8080",
		provider.EnvAutoCompactWindow + "=900000",
		provider.EnvMaxContextTokens + "=1000000",
		provider.EnvAutoCompactPct + "=90",
		"CUSTOM_SETTING=from-shell",
		"ANTHROPIC_API_KEY=sk-from-shell",
		"CCL_DEBUG_LOG=/tmp/mine.log",
	}

	values := map[string]string{}
	for _, entry := range buildProcessEnv(inherited, settings, false) {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for _, dropped := range []string{
		provider.EnvAutoCompactWindow,
		provider.EnvMaxContextTokens,
		provider.EnvAutoCompactPct,
		"CUSTOM_SETTING",
		"ANTHROPIC_API_KEY",
	} {
		if value, present := values[dropped]; present {
			t.Errorf("inherited %s=%q reached the session; the settings file is authoritative", dropped, value)
		}
	}
	// ccl's own variables and unrelated environment stay.
	if values["CCL_DEBUG_LOG"] != "/tmp/mine.log" {
		t.Errorf("ccl's own variable was dropped: %#v", values)
	}
	if values["PATH"] != "/usr/bin" || values["HTTPS_PROXY"] != "http://corp:8080" {
		t.Errorf("unrelated environment was damaged: %#v", values)
	}
}

func TestBuildProcessEnvExportsManagedContextVars(t *testing.T) {
	settings := settingsJSON{Env: map[string]string{
		provider.EnvMaxContextTokens:  "500000",
		provider.EnvAutoCompactWindow: "500000",
		provider.EnvAutoCompactPct:    "80",
	}}
	inherited := []string{"PATH=/usr/bin", provider.EnvAutoCompactPct + "=10", "HOME=/root"}

	env := buildProcessEnv(inherited, settings, false)
	values := map[string]string{}
	seen := map[string]int{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
		seen[key]++
	}
	if values[provider.EnvMaxContextTokens] != "500000" ||
		values[provider.EnvAutoCompactWindow] != "500000" {
		t.Fatalf("managed context vars were not exported: %#v", values)
	}
	// A ccl-managed value must replace the ambient one, not duplicate it.
	if values[provider.EnvAutoCompactPct] != "80" || seen[provider.EnvAutoCompactPct] != 1 {
		t.Fatalf("pct override = %q (%d entries), want a single managed value", values[provider.EnvAutoCompactPct], seen[provider.EnvAutoCompactPct])
	}
	if values["PATH"] != "/usr/bin" || values["HOME"] != "/root" {
		t.Fatalf("inherited environment was damaged: %#v", values)
	}
}

func TestBuildProcessEnvKeepsUnrelatedEnvironment(t *testing.T) {
	inherited := []string{"PATH=/usr/bin"}
	env := buildProcessEnv(inherited, settingsJSON{Env: map[string]string{}}, false)
	if len(env) != 1 || env[0] != "PATH=/usr/bin" {
		t.Fatalf("env = %#v, want the inherited entry kept", env)
	}
}

func TestBuildEnvUsesProviderCatalogDisplayNames(t *testing.T) {
	p := provider.Provider{
		Type:          "anthropic",
		OpusModel:     "cmodel[1m]",
		SonnetModel:   "kmodel_latest",
		HaikuModel:    "gm51model",
		CustomModelID: "dfmodel",
		Env: map[string]string{
			"ANTHROPIC_MODEL": "cmodel[1m]",
			SubagentModelEnv:  "dfmodel",
		},
	}
	env := buildEnvWithModelNames(p, "https://example.test", false, map[string]string{
		"cmodel":        "Cantus",
		"kmodel_latest": "Kimi-K3",
		"gm51model":     "GLM-5.2",
		"dfmodel":       "DeepSeek-V4-Flash",
	})

	want := map[string]string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL":              "Cantus[1m]",
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":         "Cantus (1M)",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":            "Kimi-K3",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME":       "Kimi-K3",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":             "GLM-5.2",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME":        "GLM-5.2",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":             "DeepSeek-V4-Flash",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        "DeepSeek-V4-Flash",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Custom provider model",
		"CLAUDE_CODE_MODEL_ID":                      "DeepSeek-V4-Flash",
		SubagentModelEnv:                            "DeepSeek-V4-Flash",
		"ANTHROPIC_MODEL":                           "Cantus[1m]",
	}
	for key, expected := range want {
		if env[key] != expected {
			t.Errorf("%s = %q, want %q", key, env[key], expected)
		}
	}
}

func TestCatalogModelOverridesUseRequestAliases(t *testing.T) {
	overrides := catalogModelOverrides(map[string]string{
		"claude-opus": "cmodel[1m]",
		"claude-fast": "dfmodel",
	}, map[string]string{
		"cmodel":  "Cantus",
		"dfmodel": "DeepSeek-V4-Flash",
	})
	if overrides["claude-opus"] != "Cantus[1m]" || overrides["claude-fast"] != "DeepSeek-V4-Flash" {
		t.Fatalf("model overrides = %#v", overrides)
	}
}
