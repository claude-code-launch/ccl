package codex

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
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
		writeMessage(w, []map[string]any{{"type": "text", "text": "unused"}})
	}, "anthropic", Options{Desktop: true})
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
