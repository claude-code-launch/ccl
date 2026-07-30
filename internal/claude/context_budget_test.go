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

func TestApplyContextPolicyDropsCclPresets(t *testing.T) {
	presets := []map[string]string{
		{provider.EnvMaxContextTokens: "1000000", provider.EnvAutoCompactWindow: "900000"},
		{provider.EnvMaxContextTokens: "500000", provider.EnvAutoCompactWindow: "400000"},
		{provider.EnvMaxContextTokens: "300000", provider.EnvAutoCompactWindow: "200000"},
		{provider.EnvAutoCompactWindow: "1000000", provider.EnvAutoCompactPct: "90"},
		{provider.EnvAutoCompactWindow: "1000000"},
	}
	for _, preset := range presets {
		env := map[string]string{"KEEP": "yes"}
		for key, value := range preset {
			env[key] = value
		}
		if !applyContextPolicy(env, provider.Provider{Name: "p"}) {
			t.Fatalf("preset %#v was not recognized", preset)
		}
		for _, key := range provider.ManagedContextEnvKeys() {
			if _, present := env[key]; present {
				t.Errorf("preset %#v left %s behind", preset, key)
			}
		}
		if env["KEEP"] != "yes" {
			t.Errorf("preset %#v removed unrelated env", preset)
		}
	}
}

func TestApplyContextPolicyKeepsDeliberateValues(t *testing.T) {
	// Not one of the presets ccl used to write, so it is the user's own number.
	env := map[string]string{
		provider.EnvMaxContextTokens:  "1050000",
		provider.EnvAutoCompactWindow: "840000",
	}
	if applyContextPolicy(env, provider.Provider{Name: "p"}) {
		t.Fatal("a custom value must not be treated as a ccl preset")
	}
	if env[provider.EnvMaxContextTokens] != "1050000" || env[provider.EnvAutoCompactWindow] != "840000" {
		t.Fatalf("custom values were modified: %#v", env)
	}

	// Manual mode protects even a value that looks like an old preset.
	manual := map[string]string{
		provider.EnvContextBudgetMode: provider.ContextBudgetManual,
		provider.EnvMaxContextTokens:  "1000000",
		provider.EnvAutoCompactWindow: "900000",
	}
	p := provider.Provider{Name: "p", Env: manual}
	if applyContextPolicy(manual, p) {
		t.Fatal("manual mode must keep the configured values")
	}
	if manual[provider.EnvMaxContextTokens] != "1000000" {
		t.Fatalf("manual values were dropped: %#v", manual)
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
	if !ctx.droppedContextPreset {
		t.Error("dropping the preset was not recorded for the log")
	}
	// The [1m] marker is the sizing signal and must survive.
	if settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "gpt-5.6-sol[1m]" {
		t.Errorf("opus model = %q, want the [1m] marker preserved", settings.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
}

func TestBuildEnvDropsCclDirectives(t *testing.T) {
	p := provider.Provider{
		Name:     "manual-gpt",
		Type:     "openai_responses",
		Endpoint: "https://example.test/v1",
		APIKey:   "sk-test",
		Env: map[string]string{
			provider.EnvContextBudgetMode: provider.ContextBudgetManual,
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
	// ccl writes no context env any more, so an ambient value would silently
	// reinstate the session-wide window this policy removed.
	settings := settingsJSON{Env: map[string]string{
		MaxOutputTokensEnv:  "32000",
		"ANTHROPIC_API_KEY": "sk-from-ccl",
	}}
	inherited := []string{
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://corp:8080",
		provider.EnvAutoCompactWindow + "=900000",
		provider.EnvMaxContextTokens + "=1000000",
		provider.EnvAutoCompactPct + "=90",
		MaxOutputTokensEnv + "=8000",
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
		MaxOutputTokensEnv,
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
		provider.EnvMaxContextTokens:  "1050000",
		provider.EnvAutoCompactWindow: "840000",
		provider.EnvAutoCompactPct:    "70",
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
	if values[provider.EnvMaxContextTokens] != "1050000" ||
		values[provider.EnvAutoCompactWindow] != "840000" {
		t.Fatalf("managed context vars were not exported: %#v", values)
	}
	// A ccl-managed value must replace the ambient one, not duplicate it.
	if values[provider.EnvAutoCompactPct] != "70" || seen[provider.EnvAutoCompactPct] != 1 {
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
