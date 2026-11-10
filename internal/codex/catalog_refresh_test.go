package codex

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestCatalogRefreshKeepsRoutesHistoryAndNativeRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := provider.Provider{Name: "chosen", Type: "openai", Model: "model-a[1m]"}
	g := fixtureProviderGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeChatText(w, r, "still connected")
	}, p, Options{})
	editFixtureModels(g, func(models []Model) []Model { models[0]["tool_mode"] = "code_mode_only"; return models })
	before := g.Models()[0]
	if before["context_window"] != 1_000_000 {
		t.Fatal("fixture did not start with a marked 1M model")
	}
	alias := before["slug"].(string)
	first := responseObject(t, postResponses(t, g, map[string]any{"model": alias, "input": "keep history"}))
	base, token := g.BaseURL(), g.Token()
	native := Model{"slug": "native-only", "context_window": 123456, "future_field": map[string]any{"native": true}}
	file := filepath.Join(t.TempDir(), "catalog.json")
	raw, _ := json.Marshal(Catalog{Models: append(g.Models(), native)})
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := &Overlay{CatalogPath: file}
	p.Model = "model-a" // Remove an old marker without changing the route ID.
	if err := g.RefreshMetadata(p, overlay.RefreshCatalog); err != nil {
		t.Fatal(err)
	}
	if g.Models()[0]["context_window"] != 200_000 || g.Models()[0]["tool_mode"] != "code_mode_only" {
		t.Fatal("context was not refreshed or client tool mode was lost")
	}
	p.ContextPreset = provider.ContextPresetNameBalanced800K
	if err := g.RefreshMetadata(p, overlay.RefreshCatalog); err != nil {
		t.Fatal(err)
	}
	if g.Models()[0]["context_window"] != 800_000 || g.Models()[0]["auto_compact_token_limit"] != 680_000 || g.BaseURL() != base || g.Token() != token {
		t.Fatal("catalog refresh changed the connection or missed compaction sizing")
	}
	updated, _ := os.ReadFile(file)
	catalog, err := ParseCatalog(updated)
	if err != nil {
		t.Fatal(err)
	}
	originalNative, _ := json.Marshal(native)
	actualNative, _ := json.Marshal(catalog.Models[1])
	if string(originalNative) != string(actualNative) {
		t.Fatalf("native model changed: %s", actualNative)
	}
	second := responseObject(t, postResponses(t, g, map[string]any{"model": alias, "previous_response_id": first["id"], "input": "continue"}))
	if second["status"] != "completed" {
		t.Fatal("catalog refresh invalidated conversation history")
	}
	p.ContextPreset = provider.ContextPresetNameBalanced500K
	if err := g.RefreshMetadata(p, func([]Model) error { return errors.New("disk unavailable") }); err == nil || g.Models()[0]["context_window"] != 800_000 {
		t.Fatal("failed persistence replaced the last published snapshot")
	}
}

func TestCatalogRefreshUpdatesEffortWithoutRacingReaders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMetadata := func(values string) {
		t.Helper()
		body := []byte(`{"fixture":{"models":{"model-a":{"reasoning":true,"reasoning_options":[{"type":"effort","values":` + values + `}]}}}}`)
		if err := fsutil.WriteFileAtomic(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata(`["low","high"]`)
	p := provider.Provider{Name: "chosen", Type: "openai", Model: "model-a", ModelsDevProvider: "fixture"}
	g := fixtureProviderGateway(t, func(http.ResponseWriter, *http.Request) {}, p, Options{})
	writeMetadata(`["low","high","max"]`)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				model := g.Models()[0]
				_ = model["context_window"]
				_, _ = json.Marshal(model)
			}
		})
	}
	for range 10 {
		if err := g.RefreshMetadata(p, func([]Model) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	levels, _ := json.Marshal(g.Models()[0]["supported_reasoning_levels"])
	var tiers []struct{ Effort string }
	if err := json.Unmarshal(levels, &tiers); err != nil || len(tiers) != 3 || tiers[2].Effort != "max" {
		t.Fatalf("new effort metadata was not published: %s", levels)
	}
	// A damaged optional cache does not remove the last known effort list.
	if err := fsutil.WriteFileAtomic(path, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.ContextPreset = provider.ContextPresetNameBalanced500K
	if err := g.RefreshMetadata(p, func([]Model) error { return nil }); err != nil {
		t.Fatal(err)
	}
	retained, _ := json.Marshal(g.Models()[0]["supported_reasoning_levels"])
	if string(retained) != string(levels) || g.Models()[0]["context_window"] != 500_000 {
		t.Fatal("cache corruption withdrew good metadata or blocked a context edit")
	}
}
