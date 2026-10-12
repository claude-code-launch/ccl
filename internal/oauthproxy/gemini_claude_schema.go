package oauthproxy

import (
	"encoding/json"
	"strings"
)

// normalizeClaudeUnion projects JSON Schema unions onto Antigravity's typed
// function schema carrier before properties/items are processed. In particular,
// Pydantic emits anyOf(T, null) for optional MCP arguments. Forwarding that node
// without a type makes the Claude bridge produce an invalid input_schema.
func (c geminiSchemaCleaner) normalizeClaudeUnion(node map[string]any, depth int, refStack []string) map[string]any {
	alternatives, ok := node["anyOf"].([]any)
	if !ok {
		return node
	}
	var branches []any
	nullable, _ := node["nullable"].(bool)
	for _, alternative := range alternatives {
		branch, ok := alternative.(map[string]any)
		if !ok {
			continue
		}
		branch = c.flatten(branch, depth+1, refStack)
		if kind, _ := branch["type"].(string); strings.EqualFold(kind, "null") {
			nullable = true
			continue
		}
		cleaned := c.clean(branch, depth+1, refStack)
		if cleaned["nullable"] == true {
			nullable = true
		}
		branches = append(branches, cleaned)
	}
	delete(node, "anyOf")
	if nullable {
		node["nullable"] = true
	}
	if len(branches) == 0 {
		if nullable {
			node["type"] = "null"
		}
		return node
	}
	if foldAntigravityEnumUnion(node, branches) {
		return node
	}
	// T | null has an exact OpenAPI representation: T with nullable:true.
	// A genuine mixed-type union has no equivalent single type in the private
	// Claude bridge. Prefer a structured branch and preserve every accepted
	// shape in a hint, rather than send TYPE_UNSPECIFIED or an invalid null type.
	selected := branches[0].(map[string]any)
	if len(branches) > 1 {
		for _, branch := range branches[1:] {
			candidate := branch.(map[string]any)
			if geminiSchemaBranchScore(candidate) > geminiSchemaBranchScore(selected) {
				selected = candidate
			}
		}
		appendGeminiSchemaJSONHint(node, "Accepted JSON shapes", branches)
	}
	for key, value := range selected {
		switch key {
		case "description":
			if text, ok := value.(string); ok {
				appendGeminiSchemaHint(node, text)
			}
		case "properties":
			props, _ := node[key].(map[string]any)
			merged := make(map[string]any)
			for name, schema := range props {
				merged[name] = schema
			}
			for name, schema := range value.(map[string]any) {
				if _, exists := merged[name]; !exists {
					merged[name] = schema
				}
			}
			node[key] = merged
		case "required":
			outer, _ := node[key].([]any)
			node[key] = unionStringAny(outer, value.([]any))
		default:
			if _, exists := node[key]; !exists {
				node[key] = value
			}
		}
	}
	return node
}

func geminiSchemaBranchScore(node map[string]any) int {
	switch node["type"] {
	case "object":
		return 3
	case "array":
		return 2
	default:
		return 1
	}
}

func geminiStringEnum(node map[string]any, values []any) bool {
	if node["type"] != "string" || len(values) == 0 {
		return false
	}
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	return true
}

func appendGeminiSchemaJSONHint(node map[string]any, label string, value any) {
	encoded, err := json.Marshal(value)
	if err == nil {
		appendGeminiSchemaHint(node, label+": "+string(encoded))
	}
}

func appendGeminiSchemaHint(node map[string]any, hint string) {
	if hint == "" {
		return
	}
	description, _ := node["description"].(string)
	node["description"] = strings.TrimSpace(description + "\n" + hint)
}
