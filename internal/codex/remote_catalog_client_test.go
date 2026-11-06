package codex

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestRealCodexRemoteCatalogListsCCLModels(t *testing.T) {
	home, _ := isolatedCodexClient(t)
	// Opt in explicitly; clients can disable API-key catalog discovery.
	original := []byte("[features]\napi_key_model_discovery = true\n")
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) { t.Error("model/list triggered inference") }, "anthropic", Options{Independent: true, CatalogMode: CatalogRemote})
	overlay, err := Plan(context.Background(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "model_catalog_json") || !strings.Contains(string(raw), "model_catalog_url") {
		t.Fatalf("remote catalog config = %s", raw)
	}
	models := readClientModelList(t, home)
	found := false
	for _, model := range models {
		if model.Model == "ccl/chosen/model-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("app-server did not read the remote CCL catalog: %+v", models)
	}
	if err := overlay.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	// Journal restoration preserves the original settings (including comments).
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(string(original)) {
		t.Fatalf("remote overlay did not restore settings: %s", raw)
	}
}

func TestRemoteCatalogRestoresLocalSourceAndRejectsShared(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	catalog := filepath.Join(home, "original.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"native-only"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	original := "model_catalog_json = '" + filepath.ToSlash(catalog) + "'\n"
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "anthropic", Options{Independent: true, CatalogMode: CatalogRemote})
	overlay, err := Plan(t.Context(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "model_catalog_json") {
		t.Fatal("local source still overrides URL")
	}
	if err := overlay.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	fields, err := configFields(raw)
	if err != nil || fields["model_catalog_json"] != filepath.ToSlash(catalog) {
		t.Fatal("previous catalog source was lost")
	}
	if _, err := New(t.Context(), g.runtime, provider.Provider{Name: "chosen", Type: "anthropic"}, Options{Desktop: true, CatalogMode: CatalogRemote}); err == nil {
		t.Fatal("shared local URL accepted")
	}
}

func TestIndependentDefaultSelectionJournalRecoversBothWindows(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", home)
			path := filepath.Join(home, "config.toml")
			if err := os.WriteFile(path, []byte("model = 'native-original'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"native-original"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			g := fixtureGateway(t, func(http.ResponseWriter, *http.Request) {}, "anthropic", Options{Independent: true})
			overlay, err := Plan(t.Context(), g, filepath.Join(home, "proxy"))
			if err != nil {
				t.Fatal(err)
			}
			if err := overlay.Apply(t.Context()); err != nil {
				t.Fatal(err)
			}
			next, err := overlay.PlanModelSelection([]Model{{"slug": "ccl/next/new-model"}})
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				if err := next.ApplyModelSelection(t.Context(), overlay); err != nil {
					t.Fatal(err)
				}
				raw, _ := os.ReadFile(path)
				if !strings.Contains(string(raw), "ccl/next/new-model") {
					t.Fatal("owned default selection not rebound")
				}
			}
			// This is the journal that would survive a crash on either side of the
			// config write. It must restore the original user's model in both cases.
			if err := next.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			fields, err := configFields(raw)
			if err != nil || fields["model"] != "native-original" || strings.Contains(string(raw), "ccl_proxy") {
				t.Fatalf("selection recovery failed: %s %v", raw, err)
			}
		})
	}
}
