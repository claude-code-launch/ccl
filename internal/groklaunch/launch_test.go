package groklaunch

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestConfigOverlayConfinesAllowedModelsAndSubagentInheritance(t *testing.T) {
	raw := ConfigOverlay(provider.Provider{
		Name:        "gw",
		Type:        "openai",
		Model:       "claude-sonnet-4-6[1m], claude-opus-4-6, claude-sonnet-4-6",
		EffortLevel: "high",
	})

	var overlay map[string]any
	if err := json.Unmarshal([]byte(raw), &overlay); err != nil {
		t.Fatalf("unmarshal overlay %q: %v", raw, err)
	}
	if _, bad := overlay["ui"]; bad {
		t.Fatalf("overlay contains unsupported ui section: %v", overlay)
	}
	if _, bad := overlay["cli"]; bad {
		t.Fatalf("overlay contains unsupported cli section: %v", overlay)
	}

	models, ok := overlay["models"].(map[string]any)
	if !ok {
		t.Fatalf("models = %T, want map", overlay["models"])
	}
	for _, key := range []string{"default", "web_search", "image_description", "session_summary", "prompt_suggestion"} {
		if models[key] != "claude-sonnet-4-6" {
			t.Fatalf("models[%q] = %v, want claude-sonnet-4-6", key, models[key])
		}
	}
	if models["default_reasoning_effort"] != "high" {
		t.Fatalf("default_reasoning_effort = %v, want high", models["default_reasoning_effort"])
	}

	gotAllowed, ok := models["allowed_models"].([]any)
	if !ok {
		t.Fatalf("allowed_models = %T, want []any", models["allowed_models"])
	}
	var allowed []string
	for _, v := range gotAllowed {
		allowed = append(allowed, v.(string))
	}
	wantAllowed := []string{"claude-sonnet-4-6", "claude-sonnet-4-6[1m]", "claude-opus-4-6"}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Fatalf("allowed_models = %v, want %v", allowed, wantAllowed)
	}

	features, ok := overlay["features"].(map[string]any)
	if !ok || features["subagent_model_inheritance"] != true {
		t.Fatalf("features = %v, want subagent_model_inheritance=true", overlay["features"])
	}
}

func TestBrowserEnvironmentFailClosedAllowlist(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin",
		"GROK_HOME=/user/home/.grok",
		"GROK_CLAUDE_SKILLS_ENABLED=1",
		"GROK_CURSOR_SKILLS_ENABLED=1",
		"GROK_SUBAGENTS=true",
		"GROK_SUBAGENTS_MAX_DEPTH=3",
		"GROK_MEMORY=1",
		"GROK_MODELS_BASE_URL=https://rogue.invalid/v1",
		"GROK_CONFIG={\"ui\":{\"permission_mode\":\"ask\"}}",
		"GROK_FUTURE_SECRET_ROUTE=leak",
	}
	overrides := map[string]string{
		"CUSTOM_KEY":                 "ok",
		"GROK_CLAUDE_SKILLS_ENABLED": "0",
		"GROK_UNKNOWN_OVERRIDE":      "drop",
	}

	env, filtered := browserEnvironment(inherited, overrides, "")
	for _, want := range []string{
		"PATH=/usr/bin",
		"GROK_HOME=/user/home/.grok",
		"GROK_CLAUDE_SKILLS_ENABLED=1",
		"GROK_CURSOR_SKILLS_ENABLED=1",
		"GROK_SUBAGENTS=true",
		"GROK_SUBAGENTS_MAX_DEPTH=3",
		"GROK_MEMORY=1",
		"GROK_SANDBOX=workspace",
		"GROK_DISABLE_AUTOUPDATER=1",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("env missing %q in %v", want, env)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GROK_MODELS_BASE_URL=") ||
			strings.HasPrefix(entry, "GROK_CONFIG=") ||
			strings.HasPrefix(entry, "GROK_FUTURE_SECRET_ROUTE=") {
			t.Fatalf("env retained disallowed GROK_* var: %q", entry)
		}
	}
	if !reflect.DeepEqual(filtered, map[string]string{
		"CUSTOM_KEY":                 "ok",
		"GROK_CLAUDE_SKILLS_ENABLED": "0",
	}) {
		t.Fatalf("filtered overrides = %v", filtered)
	}

	profileEnv, _ := browserEnvironment(inherited, nil, "/isolated/profile")
	if !slices.Contains(profileEnv, "GROK_HOME=/isolated/profile") {
		t.Fatalf("expected isolated GROK_HOME in %v", profileEnv)
	}
	if slices.Contains(profileEnv, "GROK_HOME=/user/home/.grok") {
		t.Fatalf("inherited GROK_HOME leaked alongside profile: %v", profileEnv)
	}
}

func TestNativeBrowserEnvAvoidsUnsupportedGrokConfig(t *testing.T) {
	got := NativeBrowserEnv([]string{
		"PATH=/usr/bin",
		"GROK_CONFIG={\"bad\":true}",
		"GROK_CONFIG_PATH=/tmp/bad.toml",
		"GROK_SANDBOX=off",
		"GROK_DISABLE_AUTOUPDATER=0",
	}, "bypassPermissions")

	if !slices.Contains(got, "GROK_SANDBOX=workspace") {
		t.Fatalf("expected GROK_SANDBOX=workspace in %v", got)
	}
	if !slices.Contains(got, "GROK_DISABLE_AUTOUPDATER=1") {
		t.Fatalf("expected GROK_DISABLE_AUTOUPDATER=1 in %v", got)
	}
	for _, entry := range got {
		if strings.HasPrefix(entry, "GROK_CONFIG=") || strings.HasPrefix(entry, "GROK_CONFIG_PATH=") {
			t.Fatalf("unexpected GROK_CONFIG entry in NativeBrowserEnv: %q", entry)
		}
	}
}

func TestPrepareBrowserWithEmptyModeAppliesBrowserEnvironmentAndValidConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_FUTURE_FLAG", "should-be-dropped")
	t.Setenv("GROK_CLAUDE_SKILLS_ENABLED", "1")

	launch, err := PrepareBrowserWithMode(t.Context(), filepath.Join(home, "grok"), provider.Provider{
		Name:     "gw",
		Type:     "openai",
		Endpoint: "https://gw.invalid/v1",
		APIKey:   "secret-key",
		Model:    "gpt-5.4",
	}, "", "")
	if err != nil {
		t.Fatalf("PrepareBrowserWithMode: %v", err)
	}
	defer launch.Close()

	var grokCfg string
	var hasSandbox, hasDisableUpdater, hasClaudeSkills bool
	for _, entry := range launch.Env {
		switch {
		case entry == "GROK_SANDBOX=workspace":
			hasSandbox = true
		case entry == "GROK_DISABLE_AUTOUPDATER=1":
			hasDisableUpdater = true
		case entry == "GROK_CLAUDE_SKILLS_ENABLED=1":
			hasClaudeSkills = true
		case strings.HasPrefix(entry, "GROK_FUTURE_FLAG="):
			t.Fatalf("disallowed GROK_FUTURE_FLAG leaked into launch.Env")
		case strings.HasPrefix(entry, "GROK_CONFIG="):
			grokCfg = strings.TrimPrefix(entry, "GROK_CONFIG=")
		}
	}
	if !hasSandbox || !hasDisableUpdater || !hasClaudeSkills {
		t.Fatalf("missing browser env flags (sandbox=%v disableUpdater=%v claudeSkills=%v): %v",
			hasSandbox, hasDisableUpdater, hasClaudeSkills, launch.Env)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(grokCfg), &parsed); err != nil {
		t.Fatalf("invalid GROK_CONFIG %q: %v", grokCfg, err)
	}
	if _, hasUI := parsed["ui"]; hasUI {
		t.Fatalf("GROK_CONFIG must not include unsupported ui section: %s", grokCfg)
	}
	if _, hasCLI := parsed["cli"]; hasCLI {
		t.Fatalf("GROK_CONFIG must not include unsupported cli section: %s", grokCfg)
	}
}

func TestShouldSilenceBanner(t *testing.T) {
	for _, args := range [][]string{
		{"models", "--json"},
		{"inspect", "--json"},
		{"-p", "hi", "--output-format", "json"},
		{"-p", "hi", "--output-format=json"},
	} {
		if !shouldSilenceBanner(args) {
			t.Fatalf("shouldSilenceBanner(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{
		nil,
		{"models"},
		{"--", "--json"},
		{"-p", "explain --json output"},
	} {
		if shouldSilenceBanner(args) {
			t.Fatalf("shouldSilenceBanner(%v) = true, want false", args)
		}
	}
}

func TestNormalizeReasoningEffortAndConfigOverlay(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"low":     "low",
		"HIGH":    "high",
		"xhigh":   "xhigh",
		"max":     "max",
		"minimal": "minimal",
		"none":    "none",
		"ultra":   "max",
		"ULTRA":   "max",
		"bogus":   "",
	}
	for input, want := range cases {
		if got := NormalizeReasoningEffort(input); got != want {
			t.Fatalf("NormalizeReasoningEffort(%q) = %q, want %q", input, got, want)
		}
	}

	ultraOverlay := ConfigOverlay(provider.Provider{Model: "gpt-5.4", EffortLevel: "ultra"})
	if !strings.Contains(ultraOverlay, `"default_reasoning_effort":"max"`) || strings.Contains(ultraOverlay, "ultra") {
		t.Fatalf("expected ultra EffortLevel to clamp to max in GROK_CONFIG: %s", ultraOverlay)
	}

	invalidOverlay := ConfigOverlay(provider.Provider{Model: "gpt-5.4", EffortLevel: "invalid-tier"})
	if strings.Contains(invalidOverlay, "default_reasoning_effort") {
		t.Fatalf("expected invalid EffortLevel to be omitted from GROK_CONFIG: %s", invalidOverlay)
	}
}

func TestResolveGrokDefaultAndAllowedModelsWithRuntimeCatalog(t *testing.T) {
	p := provider.Provider{
		Name:          "autoclaw-acct",
		OAuthProvider: "autoclaw",
		Model:         "glm-5.3, GLM-5.3-Flash",
	}
	runtimeModels := []string{"zaicoding_glm-5.3", "zai_glm-5.3-flash"}
	modelNames := map[string]string{
		"glm-5.3":           "GLM-5.3",
		"zaicoding_glm-5.3": "GLM-5.3",
		"zai_glm-5.3-flash": "GLM-5.3-Flash",
	}

	def := resolveGrokDefaultModelWithRuntime(p, runtimeModels, modelNames)
	if def != "zaicoding_glm-5.3" {
		t.Fatalf("resolveGrokDefaultModelWithRuntime = %q, want zaicoding_glm-5.3", def)
	}

	overlay := modelOverlay(p, def, runtimeModels...)
	modelsCfg, ok := overlay["models"].(map[string]any)
	if !ok {
		t.Fatalf("models = %T, want map", overlay["models"])
	}
	if modelsCfg["default"] != "zaicoding_glm-5.3" {
		t.Fatalf("models.default = %v, want zaicoding_glm-5.3", modelsCfg["default"])
	}
	allowed, ok := modelsCfg["allowed_models"].([]string)
	if !ok {
		t.Fatalf("allowed_models = %T, want []string", modelsCfg["allowed_models"])
	}
	for _, want := range []string{"zaicoding_glm-5.3", "zai_glm-5.3-flash", "glm-5.3", "GLM-5.3-Flash"} {
		if !slices.Contains(allowed, want) {
			t.Fatalf("allowed_models missing %q: %v", want, allowed)
		}
	}

	// Second preference matches display name directly when first entry is absent from catalog.
	p2 := provider.Provider{Model: "unknown-alias, GLM-5.3-Flash"}
	if got := resolveGrokDefaultModelWithRuntime(p2, runtimeModels, modelNames); got != "zai_glm-5.3-flash" {
		t.Fatalf("display-name fallback resolved to %q, want zai_glm-5.3-flash", got)
	}

	// Completely disjoint configured model falls back to runtimeModels[0].
	p3 := provider.Provider{Model: "retired-model"}
	if got := resolveGrokDefaultModelWithRuntime(p3, runtimeModels, modelNames); got != "zaicoding_glm-5.3" {
		t.Fatalf("disjoint fallback resolved to %q, want zaicoding_glm-5.3", got)
	}
}
