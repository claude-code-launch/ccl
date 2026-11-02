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

func newModel(alias, id string, details map[string]protocol.ModelInfo, names map[string]string) Model {
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
		name = strings.TrimSpace(info.DisplayName)
	}
	if name == "" {
		name = id
	}
	levels, defaultLevel := reasoningLevels(info)
	return Model{
		"slug": alias, "display_name": name, "description": "Model " + id + " through CCL",
		"context_window": contextWindow, "max_context_window": contextWindow,
		"shell_type": "default", "visibility": "list", "supported_in_api": true, "priority": 0,
		"truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
		"input_modalities":  []string{"text"}, "base_instructions": "You are a coding assistant. Use the available tools to inspect the workspace, implement the user's request, and verify the result. Follow the user's instructions and the workspace instructions. Ask for clarification when necessary.",
		"support_verbosity": false, "default_verbosity": nil, "supports_parallel_tool_calls": false,
		"supports_reasoning_summaries": false, "supports_reasoning_summary_parameter": false,
		"supported_reasoning_levels": levels, "default_reasoning_level": defaultLevel,
		"apply_patch_tool_type": "freeform", "experimental_supported_tools": []any{},
		"supports_search_tool": false, "supports_experimental_context": false,
		"node_repl_disabled": true, "use_responses_lite": false,
	}
}

func reasoningLevels(info protocol.ModelInfo) ([]protocol.ReasoningLevel, any) {
	levels := make([]protocol.ReasoningLevel, 0, len(info.ReasoningLevels))
	seen := make(map[string]bool)
	for _, level := range info.ReasoningLevels {
		level.Effort = strings.ToLower(strings.TrimSpace(level.Effort))
		switch level.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "persistent":
		default:
			continue
		}
		if seen[level.Effort] {
			continue
		}
		seen[level.Effort] = true
		levels = append(levels, level)
	}
	if len(levels) == 0 {
		return levels, nil
	}
	chosen := strings.ToLower(strings.TrimSpace(info.DefaultReasoningLevel))
	if !seen[chosen] {
		if seen["medium"] {
			chosen = "medium"
		} else {
			chosen = levels[0].Effort
		}
	}
	return levels, chosen
}

func disambiguateModelNames(models []Model, routes map[string]Route) {
	counts := make(map[string]int)
	for _, model := range models {
		label, _ := model["display_name"].(string)
		counts[strings.ToLower(label)]++
	}
	for _, model := range models {
		label, _ := model["display_name"].(string)
		if counts[strings.ToLower(label)] < 2 {
			continue
		}
		alias, _ := model["slug"].(string)
		route, ok := routes[alias]
		if !ok {
			continue
		}
		model["display_name"] = label + " [" + route.Model + "]"
	}
}

// WorkBuddy publishes internal revisions under the same picker label. Prefer
// the formal ID matching that label; keep every route for resumed sessions.
func uniqueWorkBuddyModels(models []Model, routes map[string]Route) []Model {
	result := make([]Model, 0, len(models))
	positions := make(map[string]int)
	normalize := func(value string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, value)
	}
	canonical := func(model Model) bool {
		label, _ := model["display_name"].(string)
		alias, _ := model["slug"].(string)
		return normalize(routes[alias].Model) == normalize(label)
	}
	for _, model := range models {
		label, _ := model["display_name"].(string)
		key := strings.ToLower(strings.TrimSpace(label))
		if position, exists := positions[key]; exists {
			if canonical(model) && !canonical(result[position]) {
				result[position] = model
			}
			continue
		}
		positions[key] = len(result)
		result = append(result, model)
	}
	return result
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
	nativeLabels := make(map[string]bool)
	for _, model := range native.Models {
		id, _ := model["slug"].(string)
		if strings.HasPrefix(id, "ccl/") {
			continue
		}
		label, _ := model["display_name"].(string)
		nativeLabels[strings.ToLower(label)] = true
	}
	merged := Catalog{Models: make([]Model, 0, len(custom)+len(native.Models))}
	for _, model := range custom {
		copy := make(Model, len(model))
		for key, value := range model {
			copy[key] = value
		}
		label, _ := model["display_name"].(string)
		if nativeLabels[strings.ToLower(label)] {
			copy["display_name"] = label + " (CCL)"
		}
		merged.Models = append(merged.Models, copy)
	}
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
