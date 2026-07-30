package claude

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestManagedCompactWindowLeavesHeadroom(t *testing.T) {
	cases := map[int]int{
		0:         0,
		272_000:   217_000,
		400_000:   320_000,
		1_000_000: 800_000,
		500:       400,
	}
	for window, want := range cases {
		if got := ManagedCompactWindow(window); got != want {
			t.Errorf("ManagedCompactWindow(%d) = %d, want %d", window, got, want)
		}
		if window > 0 && ManagedCompactWindow(window) >= window {
			t.Errorf("ManagedCompactWindow(%d) left no headroom", window)
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

func TestResolveManagedContextBudgetUsesSmallestMappedWindow(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[
		{"slug":"gpt-5.6-sol","context_window":272000},
		{"slug":"gpt-5.6-luna","context_window":128000},
		{"slug":"unused-model","context_window":16000}
	]}`)

	p := provider.Provider{
		Name:          "gpt-oauth",
		Type:          "openai_responses",
		OAuthProvider: "gpt",
		Endpoint:      server.URL,
		APIKey:        "session-key",
		OpusModel:     "gpt-5.6-sol[1m]",
		HaikuModel:    "gpt-5.6-luna",
	}
	budget, ok := ResolveManagedContextBudget(p)
	if !ok {
		t.Fatal("expected the backend catalog to manage the context budget")
	}
	if budget.Window != 128_000 || budget.Model != "gpt-5.6-luna" {
		t.Fatalf("budget = %+v, want the smallest mapped window", budget)
	}
	if budget.CompactWindow != ManagedCompactWindow(128_000) {
		t.Fatalf("compact window = %d", budget.CompactWindow)
	}
	if budget.Source == "" {
		t.Fatal("budget source is empty")
	}
}

func TestResolveManagedContextBudgetSkipsNonOAuthProviders(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[{"slug":"gpt-5.6-sol","context_window":272000}]}`)
	p := provider.Provider{
		Name:      "api-key",
		Type:      "openai",
		Endpoint:  server.URL,
		APIKey:    "sk-test",
		OpusModel: "gpt-5.6-sol",
	}
	if _, ok := ResolveManagedContextBudget(p); ok {
		t.Fatal("API-key providers must keep the user's preset")
	}
}

func TestResolveManagedContextBudgetIgnoresUnmappedModels(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[{"slug":"some-other-model","context_window":272000}]}`)
	p := provider.Provider{
		OAuthProvider: "gpt",
		Endpoint:      server.URL,
		APIKey:        "session-key",
		OpusModel:     "gpt-5.6-sol",
	}
	if _, ok := ResolveManagedContextBudget(p); ok {
		t.Fatal("a catalog without the mapped model must not produce a budget")
	}
}

func TestApplyManagedContextBudgetOutranksStoredPreset(t *testing.T) {
	env := map[string]string{
		provider.EnvMaxContextTokens:  "1000000",
		provider.EnvAutoCompactWindow: "900000",
	}
	applyManagedContextBudget(env, ManagedContextBudget{Window: 272_000, CompactWindow: 217_000})
	if env[provider.EnvMaxContextTokens] != "272000" {
		t.Errorf("max context = %q, want 272000", env[provider.EnvMaxContextTokens])
	}
	if env[provider.EnvAutoCompactWindow] != "217000" {
		t.Errorf("compact window = %q, want 217000", env[provider.EnvAutoCompactWindow])
	}

	// An empty budget must leave a configured preset alone.
	kept := map[string]string{provider.EnvMaxContextTokens: "500000"}
	applyManagedContextBudget(kept, ManagedContextBudget{})
	if kept[provider.EnvMaxContextTokens] != "500000" {
		t.Errorf("preset was overwritten without a managed window: %q", kept[provider.EnvMaxContextTokens])
	}
}

func TestResolveManagedContextBudgetHonorsManualOverride(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[{"slug":"gpt-5.6-sol","context_window":272000}]}`)
	p := provider.Provider{
		OAuthProvider: "gpt",
		Endpoint:      server.URL,
		APIKey:        "session-key",
		OpusModel:     "gpt-5.6-sol",
		Env: map[string]string{
			provider.EnvContextBudgetMode: provider.ContextBudgetManual,
			provider.EnvMaxContextTokens:  "1050000",
			provider.EnvAutoCompactWindow: "840000",
		},
	}
	if _, ok := ResolveManagedContextBudget(p); ok {
		t.Fatal("manual mode must keep the configured window, even above the advertised one")
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

func TestDeclaredContextWindowSticksToSupportedSizes(t *testing.T) {
	cases := map[int]int{
		0:         0,
		128_000:   128_000, // at or below the default: declared as-is
		200_000:   200_000,
		272_000:   200_000, // in between: Claude Code's default is the safe size
		500_000:   200_000, // the grok-4.5 case
		900_000:   900_000, // 1M-class
		1_050_000: 1_050_000,
	}
	for advertised, want := range cases {
		if got := DeclaredContextWindow(advertised); got != want {
			t.Errorf("DeclaredContextWindow(%d) = %d, want %d", advertised, got, want)
		}
	}
}

func TestResolveManagedContextBudgetDeclaresSupportedWindow(t *testing.T) {
	server := codexCatalogServer(t, `{"models":[{"slug":"grok-4.5","context_window":500000}]}`)
	p := provider.Provider{
		OAuthProvider: "grok",
		Endpoint:      server.URL,
		APIKey:        "session-key",
		OpusModel:     "grok-4.5",
	}
	budget, ok := ResolveManagedContextBudget(p)
	if !ok {
		t.Fatal("expected a managed budget")
	}
	if budget.Advertised != 500_000 {
		t.Errorf("advertised = %d, want 500000", budget.Advertised)
	}
	if budget.Window != 200_000 {
		t.Errorf("declared window = %d, want the 200K default", budget.Window)
	}
	if budget.CompactWindow != ManagedCompactWindow(200_000) {
		t.Errorf("compact window = %d, want it derived from the declared window", budget.CompactWindow)
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

func TestBuildProcessEnvUntouchedWithoutManagedVars(t *testing.T) {
	inherited := []string{"PATH=/usr/bin"}
	env := buildProcessEnv(inherited, settingsJSON{Env: map[string]string{}}, false)
	if len(env) != 1 || env[0] != "PATH=/usr/bin" {
		t.Fatalf("env = %#v, want the inherited slice unchanged", env)
	}
}
