package codex

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func pointer(s string) *string { return &s }

func TestCodexRootEditsPreserveUnrelatedTOML(t *testing.T) {
	for _, original := range []string{
		"# header\r\n\"openai_base_url\" = 'https://old.invalid' # keep\r\n[projects.\"/tmp/a\"]\r\ntrust_level = 'trusted'\r\n",
		"openai_base_url = \"\"\"https://old.invalid\"\"\"\n[model_providers.other]\nopenai_base_url = 'untouched'\n",
	} {
		result, err := editRoot([]byte(original), map[string]*string{"openai_base_url": pointer("http://127.0.0.1:1234/v1"), "model_catalog_json": pointer(`C:\Users\test\models.json`)})
		if err != nil {
			t.Fatal(err)
		}
		fields, err := configFields(result)
		if err != nil {
			t.Fatal(err)
		}
		if fields["openai_base_url"] != "http://127.0.0.1:1234/v1" || fields["model_catalog_json"] != `C:\Users\test\models.json` {
			t.Fatalf("wrong root fields: %v", fields)
		}
		if index := strings.Index(original, "["); index >= 0 && !strings.Contains(string(result), original[index:]) {
			t.Fatal("unrelated table changed")
		}
		if strings.Contains(original, "# keep") && !strings.Contains(string(result), "# keep\r\n") {
			t.Fatal("inline comment changed")
		}
	}
}

func TestCodexOverlayRestoresOnlyUnchangedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	catalog := filepath.Join(dir, "ccl-models.json")
	original := "# user settings\nmodel = 'native-model'\nopenai_base_url = 'https://original.invalid/v1'\n[projects.\"/tmp/work\"]\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := &Overlay{Path: path, CatalogPath: catalog, Changes: []ConfigChange{{Key: "openai_base_url", Before: pointer("https://original.invalid/v1"), Applied: pointer("http://127.0.0.1:1234/v1")}, {Key: "model_catalog_json", Applied: pointer(catalog)}}}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	data, err := editRoot(data, map[string]*string{"openai_base_url": pointer("https://user-edit.invalid/v1")})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("# user edit while proxy is running\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := overlay.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	fields, _ := configFields(data)
	if fields["openai_base_url"] != "https://user-edit.invalid/v1" {
		t.Fatal("user's later routing edit was lost")
	}
	if _, exists := fields["model_catalog_json"]; exists {
		t.Fatal("owned catalog key was not restored")
	}
	if !strings.Contains(string(data), "# user edit while proxy is running") || !strings.Contains(string(data), "trust_level = 'trusted'") {
		t.Fatal("unrelated user content was lost")
	}
	if _, err := os.Stat(catalog); !os.IsNotExist(err) {
		t.Fatal("unreferenced generated catalog retained")
	}
	if err := overlay.Restore(context.Background()); err != nil {
		t.Fatal("restore is not idempotent:", err)
	}
}

func TestCodexOverlayDoesNotOverwriteConcurrentStartupEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("openai_base_url = 'https://concurrent.invalid'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := &Overlay{Path: path, Changes: []ConfigChange{{Key: "openai_base_url", Before: pointer("https://before.invalid"), Applied: pointer("http://127.0.0.1:1234/v1")}}}
	if err := overlay.Apply(context.Background()); err == nil {
		t.Fatal("concurrent change overwritten")
	}
}

func TestCodexMergedCatalogRetainsNativeMetadata(t *testing.T) {
	native := Catalog{Models: []Model{{"slug": "native-a", "future_field": map[string]any{"important": true}}, {"slug": "ccl/old/model", "display_name": "stale"}}}
	custom := []Model{{"slug": "ccl/new/model", "display_name": "new"}}
	merged, err := MergeCatalog(native, custom)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Models) != 2 || merged.Models[1]["future_field"] == nil {
		t.Fatal("native metadata was replaced or stale CCL route retained")
	}
}

func TestCodexPlanKeepsNativeProviderProfileAndEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	original := "# native\nprofile = 'work'\nopenai_base_url = 'https://native-gateway.invalid/v1'\n[profiles.work]\nmodel_provider = 'openai'\nmodel = 'native-model'\n"
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"native-model","future_property":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeChatText(w, r, "unused")
	}, "openai", Options{Desktop: true})
	overlay, err := Plan(context.Background(), g, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if g.options.OpenAIURL != "https://native-gateway.invalid/v1" || g.options.ChatGPTURL != g.options.OpenAIURL {
		t.Fatal("original native upstream changed")
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "profile = 'work'") || !strings.Contains(string(data), "model_provider = 'openai'") {
		t.Fatal("native provider/profile was replaced")
	}
	if err := overlay.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "# native") || !strings.Contains(string(data), "https://native-gateway.invalid/v1") {
		t.Fatal("native configuration was not restored")
	}
}

func TestDesktopExecutableCandidatesSupportCodexCLIPackageLayout(t *testing.T) {
	home := t.TempDir()
	cliDir := filepath.Join(home, "Applications", "ChatGPT.app", "Contents", "Resources", "codex-cli")
	if err := os.MkdirAll(filepath.Join(cliDir, "custom-bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	entryPath := filepath.Join(cliDir, "custom-bin", "codex")
	if err := os.WriteFile(entryPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "codex-package.json"), []byte(`{"layoutVersion":1,"entrypoint":"custom-bin/codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	darwinCandidates := desktopExecutableCandidates("darwin", home, "")
	if !slices.Contains(darwinCandidates, entryPath) || !slices.Contains(darwinCandidates, filepath.Join(cliDir, "bin", "codex")) || !slices.Contains(darwinCandidates, filepath.Join(cliDir, "CodexCLI.app", "Contents", "MacOS", "codex")) {
		t.Fatalf("missing darwin codex-cli candidates: %v", darwinCandidates)
	}

	// Reject path traversal in codex-package.json.
	traversalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(traversalDir, "codex-package.json"), []byte(`{"entrypoint":"../outside/codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cliPackageEntrypoint(traversalDir); got != "" {
		t.Fatalf("expected traversal entrypoint to be rejected, got %q", got)
	}

	localAppData := t.TempDir()
	winCandidates := desktopExecutableCandidates("windows", "", localAppData)
	wantWin := filepath.Join(localAppData, "Programs", "ChatGPT", "resources", "codex-cli", "bin", "codex.exe")
	if !slices.Contains(winCandidates, wantWin) {
		t.Fatalf("missing windows codex-cli candidate %q in %v", wantWin, winCandidates)
	}
}

func TestNativeCatalogStrictFixtureIsolationAndStaleCacheUpgrade(t *testing.T) {
	originalBundled := bundledExecutable
	t.Cleanup(func() { bundledExecutable = originalBundled })

	// 1. Synthetic unit-test fixtures (no client_version) must NEVER invoke bundledExecutable.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"synthetic-native"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bundledCalls := 0
	bundledExecutable = func() (string, error) {
		bundledCalls++
		return "", errors.New("unexpected bundled lookup")
	}
	catalog, err := nativeCatalog(context.Background(), home, map[string]any{})
	if err != nil || bundledCalls != 0 || len(catalog.Models) != 1 || catalog.Models[0]["slug"] != "synthetic-native" {
		t.Fatalf("synthetic cache leaked to host binary: calls=%d err=%v catalog=%+v", bundledCalls, err, catalog)
	}

	// 2. Real stale cache (client_version set, missing code_mode_only) falls back gracefully if bundled fails.
	staleHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(staleHome, "models_cache.json"), []byte(`{"client_version":"0.90.0","models":[{"slug":"gpt-5-old"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err = nativeCatalog(context.Background(), staleHome, map[string]any{})
	if err != nil || bundledCalls == 0 || catalog.Models[0]["slug"] != "gpt-5-old" {
		t.Fatalf("stale cache did not consult or fall back cleanly: calls=%d err=%v catalog=%+v", bundledCalls, err, catalog)
	}

	if runtime.GOOS == "windows" {
		return
	}

	// 3. When bundled CLI is available, stale real cache and CCL-only leftover catalogs upgrade to code_mode_only.
	fakeCodex := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\necho '{\"models\":[{\"slug\":\"gpt-5.4\",\"tool_mode\":\"code_mode_only\",\"shell_type\":\"unified_exec\",\"node_repl_auto_review_required\":true}]}'\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	bundledExecutable = func() (string, error) { return fakeCodex, nil }

	upgraded, err := nativeCatalog(context.Background(), staleHome, map[string]any{})
	if err != nil || upgraded.Models[0]["tool_mode"] != "code_mode_only" || upgraded.Models[0]["shell_type"] != "unified_exec" {
		t.Fatalf("stale cache was not upgraded from bundled catalog: err=%v catalog=%+v", err, upgraded)
	}

	cclOnlyPath := filepath.Join(t.TempDir(), "independent-leftover.json")
	if err := os.WriteFile(cclOnlyPath, []byte(`{"models":[{"slug":"ccl/prev/model-a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := nativeCatalog(context.Background(), t.TempDir(), map[string]any{"model_catalog_json": cclOnlyPath})
	if err != nil || recovered.Models[0]["slug"] != "gpt-5.4" || recovered.Models[0]["tool_mode"] != "code_mode_only" {
		t.Fatalf("CCL-only leftover catalog did not recover native catalog: err=%v catalog=%+v", err, recovered)
	}
}
