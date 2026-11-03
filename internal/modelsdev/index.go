package modelsdev

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Index retains only reasoning metadata and exact aliases from a catalog.
// It is immutable after construction and safe for concurrent lookups.
type Index struct {
	providers map[string]map[string]reasoningEntry
	aliases   map[string]reasoningEntry
}

type reasoningEntry struct {
	efforts   []string
	priority  int
	ambiguous bool
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
				addReasoningAlias(local, alias, efforts, 0)
				addReasoningAlias(index.aliases, alias, efforts, priority*2)
				addReasoningAlias(index.aliases, providerID+"/"+alias, efforts, priority*2)
			}
			if _, bare, ok := strings.Cut(model.CanonicalID, "/"); ok {
				addReasoningAlias(index.aliases, bare, efforts, priority*2)
			}
			addReasoningAlias(local, model.Name, efforts, 1)
			addReasoningAlias(index.aliases, model.Name, efforts, priority*2+1)
		}
	}
	if len(index.aliases) == 0 {
		return nil, fmt.Errorf("models.dev metadata has no models")
	}
	return index, nil
}

func addReasoningAlias(index map[string]reasoningEntry, alias string, efforts []string, priority int) {
	alias = normalizeAlias(alias)
	if alias == "" {
		return
	}
	previous, exists := index[alias]
	if !exists || priority < previous.priority {
		index[alias] = reasoningEntry{efforts: efforts, priority: priority}
	} else if priority == previous.priority && !slices.Equal(previous.efforts, efforts) {
		previous.ambiguous = true
		index[alias] = previous
	}
}

// Efforts resolves an exact provider entry first, then a canonical/reference
// entry. Conflicting equal-priority aliases are not merged or guessed. Found
// with an empty slice means the model has no selectable reasoning tiers.
// The returned slice is a copy so callers cannot mutate the shared index.
func (index *Index) Efforts(providerID, modelID, displayName string) ([]string, bool) {
	aliases := []string{normalizeAlias(modelID), normalizeAlias(displayName)}
	if local := index.providers[normalizeAlias(providerID)]; local != nil {
		for _, alias := range aliases {
			if entry, ok := local[alias]; ok && !entry.ambiguous {
				return slices.Clone(entry.efforts), true
			}
		}
	}
	for _, alias := range aliases {
		if entry, ok := index.aliases[alias]; ok && !entry.ambiguous {
			return slices.Clone(entry.efforts), true
		}
	}
	return nil, false
}

func normalizeAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	alias = strings.TrimSuffix(alias, "[1m]")
	if strings.ContainsAny(alias, " \t\n\r\v\f") {
		alias = strings.Join(strings.Fields(alias), "-")
	}
	return alias
}
