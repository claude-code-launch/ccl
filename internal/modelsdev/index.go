package modelsdev

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Index retains reasoning, modality, and output-limit metadata and exact aliases
// from a catalog. It is immutable after construction and safe for concurrent lookups.
type Index struct {
	providers map[string]map[string]reasoningEntry
	aliases   map[string]reasoningEntry
}

type reasoningEntry struct {
	efforts             []string
	modalities          []string
	maxOutput           int
	priority            int
	ambiguous           bool
	modalitiesAmbiguous bool
	outputAmbiguous     bool
}

func newIndex(data []byte) (*Index, error) {
	var providers map[string]Provider
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("decode models.dev metadata: %w", err)
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("models.dev metadata is empty")
	}
	index := &Index{providers: make(map[string]map[string]reasoningEntry), aliases: make(map[string]reasoningEntry)}
	for providerID, provider := range providers {
		providerID = normalizeAlias(providerID)
		local := make(map[string]reasoningEntry)
		index.providers[providerID] = local
		for key, model := range provider.Models {
			// Metadata also serves existing routes/history. Deprecation in one
			// catalog does not withdraw a model from another provider's catalog.
			efforts := model.Efforts()
			modalities := model.InputModalities()
			maxOutput := model.Limit.Output
			// Prefer the canonical model owner's entry. OpenCode's two
			// catalogs are references when the owner has no matching row.
			priority := 3
			owner, _, _ := strings.Cut(normalizeAlias(model.CanonicalID), "/")
			switch {
			case owner != "" && owner == providerID:
				priority = 0
			case providerID == "opencode-go":
				priority = 1
			case providerID == "opencode":
				priority = 2
			}
			for _, alias := range []string{key, model.ID, model.CanonicalID} {
				if strings.TrimSpace(alias) == "" {
					continue
				}
				addModelAlias(local, alias, efforts, modalities, maxOutput, 0)
				addModelAlias(index.aliases, alias, efforts, modalities, maxOutput, priority*2)
				addModelAlias(index.aliases, providerID+"/"+alias, efforts, modalities, maxOutput, priority*2)
			}
			if _, bare, ok := strings.Cut(model.CanonicalID, "/"); ok {
				addModelAlias(index.aliases, bare, efforts, modalities, maxOutput, priority*2)
			}
			addModelAlias(local, model.Name, efforts, modalities, maxOutput, 1)
			addModelAlias(index.aliases, model.Name, efforts, modalities, maxOutput, priority*2+1)
		}
	}
	if len(index.aliases) == 0 {
		return nil, fmt.Errorf("models.dev metadata has no models")
	}
	return index, nil
}

func addModelAlias(index map[string]reasoningEntry, alias string, efforts, modalities []string, maxOutput, priority int) {
	alias = normalizeAlias(alias)
	if alias == "" {
		return
	}
	previous, exists := index[alias]
	if !exists || priority < previous.priority {
		index[alias] = reasoningEntry{
			efforts:    efforts,
			modalities: modalities,
			maxOutput:  maxOutput,
			priority:   priority,
		}
		return
	}
	if priority == previous.priority {
		if !slices.Equal(previous.efforts, efforts) {
			previous.ambiguous = true
		}
		if len(modalities) > 0 {
			if len(previous.modalities) == 0 {
				previous.modalities = modalities
			} else if !slices.Equal(previous.modalities, modalities) {
				previous.modalitiesAmbiguous = true
			}
		}
		if maxOutput > 0 {
			if previous.maxOutput == 0 {
				previous.maxOutput = maxOutput
			} else if previous.maxOutput != maxOutput {
				previous.outputAmbiguous = true
			}
		}
		index[alias] = previous
	}
}

// Efforts resolves an exact provider entry first, then a canonical/reference
// entry. Conflicting equal-priority aliases are not merged or guessed. Found
// with an empty slice means the model has no selectable reasoning tiers.
// The returned slice is a copy so callers cannot mutate the shared index.
func (index *Index) Efforts(providerID, modelID, displayName string) ([]string, bool) {
	if entry, ok := index.lookup(providerID, modelID, displayName, func(e reasoningEntry) bool { return !e.ambiguous }); ok {
		return slices.Clone(entry.efforts), true
	}
	return nil, false
}

// InputModalities resolves advertised input modalities (["text"] or ["text", "image"])
// for a model when models.dev unambiguously declares them. Unknown models return (nil, false).
func (index *Index) InputModalities(providerID, modelID, displayName string) ([]string, bool) {
	if entry, ok := index.lookup(providerID, modelID, displayName, func(e reasoningEntry) bool {
		return !e.modalitiesAmbiguous && len(e.modalities) > 0
	}); ok {
		return slices.Clone(entry.modalities), true
	}
	return nil, false
}

// MaxOutputTokens resolves the advertised maximum output token budget when known.
func (index *Index) MaxOutputTokens(providerID, modelID, displayName string) (int, bool) {
	if entry, ok := index.lookup(providerID, modelID, displayName, func(e reasoningEntry) bool {
		return !e.outputAmbiguous && e.maxOutput > 0
	}); ok {
		return entry.maxOutput, true
	}
	return 0, false
}

func (index *Index) lookup(providerID, modelID, displayName string, accept func(reasoningEntry) bool) (reasoningEntry, bool) {
	if index == nil {
		return reasoningEntry{}, false
	}
	aliases := []string{normalizeAlias(modelID), normalizeAlias(displayName)}
	if local := index.providers[normalizeAlias(providerID)]; local != nil {
		for _, alias := range aliases {
			if alias == "" {
				continue
			}
			if entry, ok := local[alias]; ok && accept(entry) {
				return entry, true
			}
		}
	}
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		if entry, ok := index.aliases[alias]; ok && accept(entry) {
			return entry, true
		}
	}
	return reasoningEntry{}, false
}

func normalizeAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	alias = strings.TrimSuffix(alias, "[1m]")
	if strings.ContainsAny(alias, " \t\n\r\v\f") {
		alias = strings.Join(strings.Fields(alias), "-")
	}
	return alias
}
