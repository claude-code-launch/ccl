package codex

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"strings"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type catalogSnapshot struct{ models []Model }

var refreshedCatalogFields = []string{
	"context_window", "max_context_window", "auto_compact_token_limit",
	"supported_reasoning_levels", "default_reasoning_level",
}

func (g *Gateway) Models() []Model {
	if snapshot := g.modelSnapshot.Load(); snapshot != nil {
		return cloneCatalogModels(snapshot.models)
	}
	// Plan can still be inheriting the native client's tool mode while an
	// already-open client reconnects to this newly bound listener.
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	return g.modelsLocked()
}

func (g *Gateway) modelsLocked() []Model {
	models := g.models
	if snapshot := g.modelSnapshot.Load(); snapshot != nil {
		models = snapshot.models
	}
	return cloneCatalogModels(models)
}

func cloneCatalogModels(models []Model) []Model {
	copy := make([]Model, len(models))
	for i, model := range models {
		copy[i] = maps.Clone(model)
	}
	return copy
}

// RefreshMetadata changes client catalog settings only. Routes, authentication,
// the runtime and conversation history remain owned by the existing session.
// Persist the new catalog before publishing it to concurrent HTTP readers.
func (g *Gateway) RefreshMetadata(configured provider.Provider, persist func([]Model) error) error {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	models := g.modelsLocked()
	details := g.runtime.ModelDetails()
	if _, err := modelsdev.CachedIndex(); err == nil {
		details, _ = withModelsDevReasoning(g.runtime.Models(), details, g.runtime.ModelDisplayNames(), configured)
	} else {
		// A missing/damaged optional cache must not withdraw the last good
		// effort metadata while a context-setting edit is being applied.
		details = nil
	}
	for _, model := range models {
		alias, _ := model["slug"].(string)
		route, ok := g.routes[alias]
		if !ok {
			continue
		}
		base := provider.StripContextSuffix(route.Model)
		// The old route may contain a marker the user has since removed. Only
		// the current pool/slots decide whether that model still has 1M.
		applyProviderContext(model, base, configured)
		info, ok := details[route.Model]
		if !ok {
			info = details[strings.ToLower(route.Model)]
		}
		if !route.NativeResponses && info.ReasoningKnown {
			model["supported_reasoning_levels"], model["default_reasoning_level"] = reasoningLevels(info)
		}
	}
	if err := persist(models); err != nil {
		return err
	}
	g.modelSnapshot.Store(&catalogSnapshot{models: models})
	return nil
}

// RefreshCatalog updates only CCL-owned metadata in the existing file. It never
// rewrites config.toml, rotates independent credentials, or reconstructs native
// entries. The catalog path and overlay's restoration journal stay unchanged.
func (o *Overlay) RefreshCatalog(models []Model) error {
	raw, err := os.ReadFile(o.CatalogPath)
	if err != nil {
		return err
	}
	catalog, err := ParseCatalog(raw)
	if err != nil {
		return err
	}
	updates := make(map[string]Model, len(models))
	for _, model := range models {
		id, _ := model["slug"].(string)
		updates[id] = model
	}
	for _, model := range catalog.Models {
		id, _ := model["slug"].(string)
		if update, owned := updates[id]; owned {
			for _, key := range refreshedCatalogFields {
				if value, exists := update[key]; exists {
					model[key] = value
				} else {
					delete(model, key)
				}
			}
		}
	}
	updated, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if bytes.Equal(raw, updated) {
		return nil
	}
	return fsutil.WriteFileAtomic(o.CatalogPath, updated, 0o600)
}
