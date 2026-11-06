package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
)

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
	return g.RefreshProvidersMetadata([]provider.Provider{configured}, persist)
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

// ReplaceCatalog changes the complete CCL model set while preserving all
// native rows and unknown native fields from the current catalog file.
func (o *Overlay) ReplaceCatalog(models []Model) error {
	raw, err := os.ReadFile(o.CatalogPath)
	if err != nil {
		return err
	}
	catalog, err := ParseCatalog(raw)
	if err != nil {
		return err
	}
	native := Catalog{}
	for _, model := range catalog.Models {
		id, _ := model["slug"].(string)
		if !strings.HasPrefix(strings.ToLower(id), "ccl/") {
			native.Models = append(native.Models, model)
		}
	}
	combined, err := MergeCatalog(native, models)
	if err != nil {
		return err
	}
	updated, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(o.CatalogPath, append(updated, '\n'), 0o600)
}

// PlanModelSelection replaces an owned independent-mode default only when its
// route was removed. A user-edited selection is preserved. The new journal can
// recover a crash before or after ApplyModelSelection changes config.toml.
func (o *Overlay) PlanModelSelection(models []Model) (*Overlay, error) {
	next := *o
	next.Changes = append([]ConfigChange(nil), o.Changes...)
	if o.ProviderBlock == "" || len(models) == 0 {
		return &next, nil
	}
	raw, err := readConfig(o.Path)
	if err != nil {
		return nil, err
	}
	fields, err := configFields(raw)
	if err != nil {
		return nil, err
	}
	available := make(map[string]bool, len(models))
	for _, model := range models {
		id, _ := model["slug"].(string)
		available[id] = true
	}
	replacement, _ := models[0]["slug"].(string)
	for i, change := range next.Changes {
		if change.Key != "model" || change.Applied == nil || available[*change.Applied] {
			continue
		}
		if current := scopeFields(fields, change.Scope)[change.Key]; current != *change.Applied {
			continue
		}
		next.Changes[i].PreviousApplied = change.Applied
		value := replacement
		next.Changes[i].Applied = &value
	}
	return &next, nil
}

func (o *Overlay) ApplyModelSelection(ctx context.Context, previous *Overlay) error {
	changed := false
	for i, change := range o.Changes {
		if change.Key == "model" && change.Applied != nil && previous.Changes[i].Applied != nil && *change.Applied != *previous.Changes[i].Applied {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return withConfig(ctx, o.Path, func(raw []byte, fields map[string]any) ([]byte, error) {
		for i, change := range o.Changes {
			old := previous.Changes[i]
			if change.Key != "model" || change.Applied == nil || old.Applied == nil || *change.Applied == *old.Applied {
				continue
			}
			if current := scopeFields(fields, change.Scope)[change.Key]; current != *old.Applied {
				return nil, fmt.Errorf("Codex model selection changed during catalog refresh")
			}
			var err error
			raw, err = editScope(raw, change.Scope, map[string]*string{change.Key: change.Applied})
			if err != nil {
				return nil, err
			}
		}
		return raw, nil
	})
}
