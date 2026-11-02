package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

// Model preserves Codex's catalog fields, including fields introduced by newer
// clients. Native entries are never reconstructed from a /v1/models list.
type Model map[string]any
type Catalog struct {
	Models []Model `json:"models"`
}

func newModel(alias, id, provider string, details map[string]protocol.ModelInfo, names map[string]string) Model {
	contextWindow := 32768
	info, ok := details[id]
	if !ok {
		info, ok = details[strings.ToLower(id)]
	}
	if ok && info.ContextWindow > 0 {
		contextWindow = info.ContextWindow
	}
	name := names[id]
	if name == "" {
		name = id
	}
	return Model{
		"slug": alias, "display_name": name + " (CCL: " + provider + ")", "description": "CCL provider " + provider,
		"context_window": contextWindow, "max_context_window": contextWindow,
		"shell_type": "default", "visibility": "list", "supported_in_api": true, "priority": 0,
		"truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
		"input_modalities":  []string{"text"}, "base_instructions": "You are a coding assistant. Use the available tools to inspect the workspace, implement the user's request, and verify the result. Follow the user's instructions and the workspace instructions. Ask for clarification when necessary.",
		"support_verbosity": false, "default_verbosity": nil, "supports_parallel_tool_calls": false,
		"supports_reasoning_summaries": false, "supports_reasoning_summary_parameter": false,
		"supported_reasoning_levels": []any{}, "default_reasoning_level": nil,
		"apply_patch_tool_type": "freeform", "experimental_supported_tools": []any{},
		"supports_search_tool": false, "supports_experimental_context": false,
		"node_repl_disabled": true, "use_responses_lite": false,
	}
}

func ParseCatalog(data []byte) (Catalog, error) {
	var catalog Catalog
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if len(data) > 16<<20 || decoder.Decode(&catalog) != nil || len(catalog.Models) == 0 {
		return Catalog{}, fmt.Errorf("invalid or empty Codex model catalog")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Catalog{}, fmt.Errorf("invalid Codex model catalog suffix")
	}
	seen := make(map[string]bool)
	for _, model := range catalog.Models {
		id, _ := model["slug"].(string)
		if strings.TrimSpace(id) == "" || seen[id] {
			return Catalog{}, fmt.Errorf("invalid or duplicate Codex model slug")
		}
		seen[id] = true
	}
	return catalog, nil
}

func MergeCatalog(native Catalog, custom []Model) (Catalog, error) {
	merged := Catalog{Models: append([]Model(nil), custom...)}
	seen := make(map[string]bool)
	for _, model := range custom {
		id, _ := model["slug"].(string)
		seen[id] = true
	}
	for _, model := range native.Models {
		id, _ := model["slug"].(string)
		if strings.HasPrefix(id, "ccl/") {
			continue
		} // A prior CCL overlay is not a native route.
		if seen[id] {
			return Catalog{}, fmt.Errorf("native and CCL model catalog collision")
		}
		seen[id] = true
		merged.Models = append(merged.Models, model)
	}
	return merged, nil
}
