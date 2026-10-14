package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	tui "github.com/grindlemire/go-tui"
)

func TestMappingPanelEditsPreloadedCatalogWithoutConnectionChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    provider.Provider
	}{
		{"OAuth", provider.Provider{Type: "openai_responses", Endpoint: "oauth://codex", OAuthProvider: "gpt"}},
		{"Custom", provider.Provider{Type: "openai", Endpoint: "https://unused.invalid/v1", APIKey: "fake-key"}},
		{"models.dev", provider.Provider{Type: "modelsdev", Endpoint: "https://unused.invalid/v1", APIKey: "fake-key", ModelProtocols: map[string]string{"model-a": "openai"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			p.Name, p.Model = "mapping-test", "model-a,model-b"
			m := NewAdvancedMappingModel(&p, []string{"model-a", "model-b", "new-unmapped-model"}, nil)
			_ = m.Watchers()
			if m.live().detecting || m.runtimeReady || m.live().autoVerifyPending {
				t.Fatal("mapping panel started verification or claimed a running runtime")
			}
			if !m.connectionReady() || !m.canSave() {
				t.Fatal("preloaded model catalog is not editable/saveable")
			}
			for _, row := range []configRowKind{rowSource, rowEndpoint, rowAPIKey, rowTest, rowTestModels, rowFast, rowActive} {
				if m.mainRowIndex(row) >= 0 {
					t.Fatalf("mapping panel exposes unrelated row %v", row)
				}
			}
			for _, row := range []configRowKind{rowOpus, rowSonnet, rowHaiku, rowFable, rowCustom, rowSubagent} {
				m.cursor = m.mainRowIndex(row)
				m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
				if !m.filterFocused {
					t.Fatalf("cannot open slot picker for %v", row)
				}
				for _, r := range "new-unmapped" {
					m.handleKey(keyPress(r))
				}
				m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
				if got := m.slotModelForRow(row); got != "new-unmapped-model" {
					t.Fatalf("slot %v selected %q", row, got)
				}
			}
			if p.Model != "model-a,model-b,new-unmapped-model" {
				t.Fatalf("full catalog not retained: %q", p.Model)
			}
			m.cursor = m.mainRowIndex(rowSave)
			m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
			if !m.saveConfirmed || !m.quitRequested {
				t.Fatal("Save did not confirm the mapping")
			}
		})
	}
}

func TestMappingPanelCancelDoesNotPersist(t *testing.T) {
	for _, cancel := range []string{"Escape", "Ctrl-C", "Cancel"} {
		t.Run(cancel, func(t *testing.T) {
			p := provider.Provider{Name: "mapping-test", Type: "openai", Endpoint: "https://unused.invalid/v1", APIKey: "fake-key", Model: "model-a,model-b", OpusModel: "model-a[1m]", CustomModelID: "model-a"}
			if err := config.Save(&provider.Config{ActiveProvider: p.Name, Providers: map[string]provider.Provider{p.Name: p}}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(config.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			m := NewAdvancedMappingModel(&p, []string{"model-a", "model-b"}, nil)
			m.cursor = m.mainRowIndex(rowCustom)
			m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
			m.filterText.Set("model-b")
			m.updateFilteredPool()
			m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
			switch cancel {
			case "Escape":
				m.handleKey(tui.KeyEvent{Key: tui.KeyEscape})
			case "Ctrl-C":
				m.handleKey(tui.KeyEvent{Mod: tui.ModCtrl, Rune: 'c'})
			case "Cancel":
				m.cursor = m.mainRowIndex(rowCancel)
				m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
			}
			if saved, err := persistMappingModel(p.Name, m); err != nil || saved {
				t.Fatalf("cancel persisted: saved=%t err=%v", saved, err)
			}
			after, err := os.ReadFile(config.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("cancel changed the config file")
			}
		})
	}
}

func TestPersistMappingPreservesConcurrentProviderChanges(t *testing.T) {
	p := provider.Provider{Name: "mapping-test", Type: "openai", Endpoint: "https://old.invalid/v1", APIKey: "fake-old", Model: "model-a,model-b", CustomModelID: "model-a"}
	if err := config.Save(&provider.Config{ActiveProvider: p.Name, Providers: map[string]provider.Provider{p.Name: p}}); err != nil {
		t.Fatal(err)
	}
	m := NewAdvancedMappingModel(&p, []string{"model-a", "model-b", "new-unmapped-model"}, nil)
	m.cursor = m.mainRowIndex(rowCustom)
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	m.filterText.Set("new-unmapped")
	m.updateFilteredPool()
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	m.cursor = m.mainRowIndex(rowSave)
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if err := updateProvider(p.Name, func(latest *provider.Provider) error {
		latest.Endpoint, latest.APIKey = "https://new.invalid/v1", "fake-new"
		latest.FastMode, latest.EffortLevel = true, "high"
		latest.Env = map[string]string{"KEEP_ME": "concurrent"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if saved, err := persistMappingModel(p.Name, m); err != nil || !saved {
		t.Fatalf("save failed: %t %v", saved, err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Providers[p.Name]
	if got.CustomModelID != "new-unmapped-model" || got.Model != "model-a,model-b,new-unmapped-model" {
		t.Fatalf("mapping/catalog not saved: %+v", got)
	}
	if got.Endpoint != "https://new.invalid/v1" || got.APIKey != "fake-new" || !got.FastMode || got.EffortLevel != "high" || got.Env["KEEP_ME"] != "concurrent" {
		t.Fatal("mapping overwrote concurrent connection/runtime changes")
	}
	if cfg.ActiveProvider != p.Name {
		t.Fatal("mapping changed provider selection")
	}
}

func TestMappingPanelSaveLabelMouseHitBothLanguages(t *testing.T) {
	defer locale.SetLanguage("en")
	for _, language := range []string{"en", "zh"} {
		locale.SetLanguage(language)
		p := provider.Provider{Name: "mapping-test", Type: "openai", Model: "model-a"}
		m := NewAdvancedMappingModel(&p, []string{"model-a"}, nil)
		lines := strings.Split(renderView(t, m), "\n")
		label := locale.T("保存映射", "Save Mapping")
		found := false
		for y, line := range lines {
			if !strings.Contains(line, label) {
				continue
			}
			row, ok := rowAtLineAt(lines, y, -1)
			if !ok || row != rowSave {
				t.Fatalf("%s save label is not clickable", language)
			}
			found = true
		}
		if !found {
			t.Fatalf("%s missing Save Mapping", language)
		}
	}
}

func TestEmptyMappingPanelCannotSave(t *testing.T) {
	p := provider.Provider{Name: "mapping-test", Type: "openai"}
	m := NewAdvancedMappingModel(&p, nil, nil)
	if m.canSave() || m.connectionReady() {
		t.Fatal("empty catalog treated as ready")
	}
	m.cursor = m.mainRowIndex(rowSave)
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if m.saveConfirmed || m.quitRequested {
		t.Fatal("empty catalog accepted Save")
	}
}
