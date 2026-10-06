package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const geminiSchemaMaxDepth = 32

// sanitizeGeminiSchema rewrites a JSON Schema (as sent by Claude Code in
// tools[].input_schema) into the OpenAPI subset accepted by Gemini/Antigravity
// function declarations. Gemini rejects unknown keywords outright ($schema,
// propertyNames, additionalProperties, $ref, const, ...), so unsupported
// keywords are dropped or rewritten rather than forwarded.
//
// On any parse failure the original bytes are returned unchanged.
func sanitizeGeminiSchema(raw []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return raw
	}
	object, ok := root.(map[string]any)
	if !ok {
		return raw
	}
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if table, ok := object[key].(map[string]any); ok {
			for name, def := range table {
				defs[key+"/"+name] = def
			}
		}
	}
	cleaner := geminiSchemaCleaner{defs: defs}
	cleaned := cleaner.clean(object, 0, nil)
	if len(cleaned) == 0 {
		// A declaration's parameters must describe an object; a schema whose every
		// keyword was dropped would otherwise reach Gemini as an empty object.
		cleaned = map[string]any{"type": "object"}
	}
	out, err := json.Marshal(cleaned)
	if err != nil {
		return raw
	}
	return out
}

type geminiSchemaCleaner struct {
	defs map[string]any
}

// geminiAllowedSchemaKeys are the keywords forwarded after cleaning.
var geminiAllowedSchemaKeys = map[string]bool{
	"type": true, "description": true, "nullable": true, "enum": true,
	"properties": true, "required": true, "items": true, "anyOf": true,
	"minItems": true, "maxItems": true, "minimum": true, "maximum": true,
	"minLength": true, "maxLength": true, "format": true,
}

func (c geminiSchemaCleaner) clean(node map[string]any, depth int, refStack []string) map[string]any {
	if node == nil {
		return nil
	}
	if depth > geminiSchemaMaxDepth {
		return map[string]any{"type": "object"}
	}
	node = c.flatten(node, depth, refStack)

	// const -> single-value enum.
	if value, ok := node["const"]; ok {
		if _, hasEnum := node["enum"]; !hasEnum {
			node["enum"] = []any{value}
		}
	}
	// oneOf is treated as anyOf.
	if one, ok := node["oneOf"].([]any); ok {
		if _, hasAny := node["anyOf"]; !hasAny {
			node["anyOf"] = one
		}
	}

	// type may be an array such as ["string","null"].
	if list, ok := node["type"].([]any); ok {
		var kinds []string
		for _, item := range list {
			name, _ := item.(string)
			if name == "null" {
				node["nullable"] = true
			} else if name != "" {
				kinds = append(kinds, name)
			}
		}
		switch len(kinds) {
		case 0:
			delete(node, "type")
		case 1:
			node["type"] = kinds[0]
		default:
			delete(node, "type")
			if _, hasAny := node["anyOf"]; !hasAny {
				alternatives := make([]any, 0, len(kinds))
				for _, kind := range kinds {
					alternatives = append(alternatives, map[string]any{"type": kind})
				}
				node["anyOf"] = alternatives
			}
		}
	}
	if kind, ok := node["type"].(string); ok {
		node["type"] = strings.ToLower(kind)
	}
	if _, hasType := node["type"]; !hasType {
		switch {
		case node["properties"] != nil:
			node["type"] = "object"
		case node["items"] != nil:
			node["type"] = "array"
		}
	}

	// Gemini only supports string enums.
	if values, ok := node["enum"].([]any); ok {
		strs := make([]any, 0, len(values))
		for _, value := range values {
			switch typed := value.(type) {
			case nil:
				node["nullable"] = true
			case string:
				strs = append(strs, typed)
			case json.Number:
				strs = append(strs, typed.String())
			default:
				strs = append(strs, fmt.Sprint(typed))
			}
		}
		if len(strs) == 0 {
			delete(node, "enum")
		} else {
			node["enum"] = strs
			node["type"] = "string"
		}
	}

	if props, ok := node["properties"].(map[string]any); ok {
		cleanedProps := make(map[string]any, len(props))
		for name, value := range props {
			child, _ := value.(map[string]any)
			if child == nil {
				child = map[string]any{}
			}
			cleanedProps[name] = c.clean(child, depth+1, refStack)
		}
		if len(cleanedProps) == 0 {
			delete(node, "properties")
		} else {
			node["properties"] = cleanedProps
		}
	}
	if kind, _ := node["type"].(string); kind != "object" {
		delete(node, "properties")
		delete(node, "required")
	}

	// required may only name declared properties.
	if required, ok := node["required"].([]any); ok {
		props, _ := node["properties"].(map[string]any)
		kept := make([]any, 0, len(required))
		for _, item := range required {
			if name, ok := item.(string); ok {
				if _, declared := props[name]; declared {
					kept = append(kept, name)
				}
			}
		}
		if len(kept) == 0 {
			delete(node, "required")
		} else {
			node["required"] = kept
		}
	}

	if items, ok := node["items"]; ok {
		switch typed := items.(type) {
		case map[string]any:
			node["items"] = c.clean(typed, depth+1, refStack)
		case []any:
			// Tuple form: Gemini accepts a single item schema only.
			if len(typed) > 0 {
				if first, ok := typed[0].(map[string]any); ok {
					node["items"] = c.clean(first, depth+1, refStack)
					break
				}
			}
			node["items"] = map[string]any{}
		default:
			node["items"] = map[string]any{}
		}
	}
	if kind, _ := node["type"].(string); kind == "array" {
		if _, ok := node["items"]; !ok {
			node["items"] = map[string]any{"type": "string"}
		}
	} else {
		delete(node, "items")
	}

	if alternatives, ok := node["anyOf"].([]any); ok {
		cleaned := make([]any, 0, len(alternatives))
		for _, item := range alternatives {
			if child, ok := item.(map[string]any); ok {
				cleaned = append(cleaned, c.clean(child, depth+1, refStack))
			}
		}
		if len(cleaned) == 0 {
			delete(node, "anyOf")
		} else {
			node["anyOf"] = cleaned
		}
	}

	// Gemini only accepts a few string formats and numeric width formats.
	if format, ok := node["format"].(string); ok {
		kind, _ := node["type"].(string)
		keep := false
		switch kind {
		case "string":
			keep = format == "date-time" || format == "enum"
		case "integer":
			keep = format == "int32" || format == "int64"
		case "number":
			keep = format == "float" || format == "double"
		}
		if !keep {
			delete(node, "format")
		}
	}

	for key := range node {
		if !geminiAllowedSchemaKeys[key] {
			delete(node, key)
		}
	}
	return node
}

// flatten resolves local $ref pointers and merges allOf members into node,
// returning a fresh map so shared definitions are never mutated.
func (c geminiSchemaCleaner) flatten(node map[string]any, depth int, refStack []string) map[string]any {
	merged := make(map[string]any, len(node))
	for key, value := range node {
		merged[key] = value
	}

	if ref, ok := merged["$ref"].(string); ok {
		delete(merged, "$ref")
		key := strings.TrimPrefix(ref, "#/")
		target, found := c.defs[key]
		cyclic := false
		for _, seen := range refStack {
			if seen == key {
				cyclic = true
			}
		}
		if targetMap, isMap := target.(map[string]any); found && isMap && !cyclic {
			resolved := c.flatten(targetMap, depth+1, append(refStack[:len(refStack):len(refStack)], key))
			for k, v := range resolved {
				if _, exists := merged[k]; !exists {
					merged[k] = v
				}
			}
		} else if _, hasType := merged["type"]; !hasType {
			merged["type"] = "object"
		}
	}

	if parts, ok := merged["allOf"].([]any); ok {
		delete(merged, "allOf")
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			flat := c.flatten(partMap, depth+1, refStack)
			for k, v := range flat {
				switch k {
				case "properties":
					props, _ := merged["properties"].(map[string]any)
					next := map[string]any{}
					for pk, pv := range props {
						next[pk] = pv
					}
					if more, ok := v.(map[string]any); ok {
						for pk, pv := range more {
							next[pk] = pv
						}
					}
					merged["properties"] = next
				case "required":
					existing, _ := merged["required"].([]any)
					extra, _ := v.([]any)
					merged["required"] = unionStringAny(existing, extra)
				default:
					if _, exists := merged[k]; !exists {
						merged[k] = v
					}
				}
			}
		}
	}
	return merged
}

func unionStringAny(a, b []any) []any {
	seen := map[string]bool{}
	var names []string
	for _, list := range [][]any{a, b} {
		for _, item := range list {
			if name, ok := item.(string); ok && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}
