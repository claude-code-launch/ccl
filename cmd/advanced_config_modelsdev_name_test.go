package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// TestModelsDevPickKeepsTheNameTheUserGave pins `ccl set oc`: picking a
// models.dev provider must not rename the draft to the catalog ID, or the save
// lands under a name the user never typed.
func TestModelsDevPickKeepsTheNameTheUserGave(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Name: "oc"})
	m.applyModelsDevProvider(testModelsDevProvider())

	if m.p.Name != "oc" {
		t.Fatalf("name after picking a models.dev provider = %q, want %q", m.p.Name, "oc")
	}
	if m.p.ModelsDevProvider != "test-gw" {
		t.Fatalf("catalog ID = %q, want %q", m.p.ModelsDevProvider, "test-gw")
	}
	if !strings.Contains(renderView(t, m), "oc  (test-gw)") {
		t.Fatalf("Provider row does not show the catalog ID next to the name:\n%s", renderView(t, m))
	}
}

// TestModelsDevPickReplacesAGeneratedName keeps the old behavior where it is an
// improvement: `ccl set` with no name gets a provider-xxxxxx placeholder, and
// the catalog ID is the better name.
func TestModelsDevPickReplacesAGeneratedName(t *testing.T) {
	m := NewAdvancedConfigModel(&provider.Provider{Name: "provider-abc234"})
	m.NameGenerated = true
	m.applyModelsDevProvider(testModelsDevProvider())

	if m.p.Name != "test-gw" || m.p.ModelsDevProvider != "test-gw" {
		t.Fatalf("generated name was kept: name=%q catalog=%q", m.p.Name, m.p.ModelsDevProvider)
	}
}

// TestModelsDevPickKeepsTheNameOfAnEditedProvider covers switching an existing
// provider to another catalog entry: the save must replace that provider, not
// add a second one under the catalog ID.
func TestModelsDevPickKeepsTheNameOfAnEditedProvider(t *testing.T) {
	p := provider.Provider{
		Name: "work", Type: "modelsdev", ModelsDevProvider: "old-gw",
		Endpoint: "https://old.example/v1", APIKey: "k",
		Model: "a", ModelProtocols: map[string]string{"a": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	m.applyModelsDevProvider(testModelsDevProvider())

	if m.p.Name != "work" || m.p.ModelsDevProvider != "test-gw" {
		t.Fatalf("edited provider: name=%q catalog=%q", m.p.Name, m.p.ModelsDevProvider)
	}
}

// TestModelsDevRefreshFollowsTheCatalogIDNotTheName pins that a renamed
// provider still refreshes against its catalog entry.
func TestModelsDevRefreshFollowsTheCatalogIDNotTheName(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "oc", ModelsDevProvider: "test-gw",
		Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "test-model", ModelProtocols: map[string]string{"test-model": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	m.live().keyVerified = true
	m.live().modelsDevRefreshPending = false

	catalog := testModelsDevProvider()
	catalog.Models["fresh"] = modelsdev.Model{ID: "fresh", Name: "Fresh"}
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{catalog},
		refreshFor: "test-gw",
	})
	if got := strings.Join(m.live().modelPool, ","); got != "test-model,fresh" {
		t.Fatalf("renamed provider was not refreshed: pool = %q", got)
	}

	// A refresh keyed by the display name is someone else's result.
	before := strings.Join(m.live().modelPool, ",")
	catalog.Models["other"] = modelsdev.Model{ID: "other", Name: "Other"}
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{catalog},
		refreshFor: "oc",
	})
	if got := strings.Join(m.live().modelPool, ","); got != before {
		t.Fatalf("a refresh for the display name was applied: %q -> %q", before, got)
	}
}

// TestModelsDevRefreshBackfillsTheCatalogID covers configs saved before
// ModelsDevProvider existed, where the name was the catalog ID: the first
// refresh records it so a later rename keeps working.
func TestModelsDevRefreshBackfillsTheCatalogID(t *testing.T) {
	p := provider.Provider{
		Type: "modelsdev", Name: "test-gw", Endpoint: "https://example.test/v1", APIKey: "key",
		Model: "test-model", ModelProtocols: map[string]string{"test-model": "openai"},
	}
	m := NewAdvancedConfigModel(&p)
	m.live().keyVerified = true
	m.live().modelsDevRefreshPending = false
	m.handleModelsDevRefreshDone(modelsDevFetchDoneMsg{
		providers:  []modelsdev.Provider{testModelsDevProvider()},
		refreshFor: "test-gw",
	})
	if p.ModelsDevProvider != "test-gw" {
		t.Fatalf("catalog ID was not backfilled: %q", p.ModelsDevProvider)
	}
}

// TestBlockedSaveExplainsWhy pins the hint under a greyed-out Save button. A
// blocked press used to do nothing visible, so the user's only way out was Esc,
// which then reported "configuration canceled".
func TestBlockedSaveExplainsWhy(t *testing.T) {
	newModelsDev := func() *AdvancedConfigModel {
		p := provider.Provider{
			Type: "modelsdev", Name: "oc", ModelsDevProvider: "test-gw",
			Endpoint: "https://example.test/v1", APIKey: "key",
			Model: "test-model", ModelProtocols: map[string]string{"test-model": "openai"},
		}
		return NewAdvancedConfigModel(&p)
	}

	m := newModelsDev()
	if m.canSave() {
		t.Fatal("an unverified models.dev key must not be savable")
	}
	if m.requestSave() || m.saveConfirmed {
		t.Fatal("requestSave confirmed an unverified key")
	}
	if view := renderView(t, m); !strings.Contains(view, "API Key 尚未验证") &&
		!strings.Contains(view, "API key not verified yet") {
		t.Fatalf("blocked save has no reason:\n%s", view)
	}

	m.live().detecting = true
	if reason := m.saveBlockedReason(); !strings.Contains(reason, "正在验证") &&
		!strings.Contains(reason, "Verifying") {
		t.Fatalf("verifying reason = %q", reason)
	}

	m.live().detecting = false
	m.live().detectionError = fmt.Errorf("HTTP 503")
	if reason := m.saveBlockedReason(); !strings.Contains(reason, "Enter") {
		t.Fatalf("failed verification reason does not say how to retry: %q", reason)
	}

	// Once verified, the button is live and the hint is gone.
	m.live().detectionError = nil
	m.live().keyVerified = true
	if !m.canSave() {
		t.Fatal("a verified models.dev key is not savable")
	}
	view := renderView(t, m)
	for _, hint := range []string{"尚未验证", "not verified yet", "验证未通过", "verification failed"} {
		if strings.Contains(view, hint) {
			t.Fatalf("saveable page still shows %q:\n%s", hint, view)
		}
	}

	custom := NewAdvancedConfigModel(&provider.Provider{Name: "fresh"})
	if reason := custom.saveBlockedReason(); !strings.Contains(reason, "Auto Configure") {
		t.Fatalf("custom provider reason = %q", reason)
	}
}
