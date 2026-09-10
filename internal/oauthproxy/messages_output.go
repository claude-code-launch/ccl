package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tidwall/gjson"
)

// Preserve JSON numbers when a protocol envelope must be edited. In particular,
// schema enum/const values must not pass through float64 on model routing.
func decodeProtocolJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values in protocol payload")
	}
	return nil
}

// Both OpenAI dialects share these json_schema fields but nest them differently.
// Like CPA's Claude-to-Codex translator, retain the schema and downgrade strict
// mode for optional properties rather than silently making them required.
func messagesStructuredOutput(output *anthropicOutput) (map[string]any, error) {
	if output == nil || len(output.Format) == 0 || string(output.Format) == "null" {
		return nil, nil
	}
	var format struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Strict      *bool           `json:"strict"`
		Schema      json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(output.Format, &format); err != nil {
		return nil, fmt.Errorf("invalid output_config.format: %w", err)
	}
	if format.Type != "json_schema" || !gjson.ParseBytes(format.Schema).IsObject() {
		return nil, fmt.Errorf("output_config.format requires type json_schema and an object schema")
	}
	if format.Name == "" {
		format.Name = "ccl_structured_output"
	}
	strict := format.Strict == nil || *format.Strict
	strict = strict && schemaSupportsStrictOutput(gjson.ParseBytes(format.Schema))
	result := map[string]any{"name": format.Name, "strict": strict, "schema": format.Schema}
	if format.Description != "" {
		result["description"] = format.Description
	}
	return result, nil
}

func schemaSupportsStrictOutput(schema gjson.Result) bool {
	if !schema.IsObject() {
		return true
	}
	properties := schema.Get("properties")
	if properties.IsObject() || schema.Get("type").String() == "object" {
		if schema.Get("additionalProperties").Type != gjson.False {
			return false
		}
		required := make(map[string]bool)
		for _, name := range schema.Get("required").Array() {
			required[name.String()] = true
		}
		for name := range properties.Map() {
			if !required[name] {
				return false
			}
		}
	}
	// Only traverse schema-bearing keywords, not enum/const example objects.
	for _, keyword := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"} {
		for _, child := range schema.Get(keyword).Map() {
			if !schemaSupportsStrictOutput(child) {
				return false
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties", "contains", "not", "if", "then", "else", "propertyNames"} {
		if !schemaSupportsStrictOutput(schema.Get(keyword)) {
			return false
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		for _, child := range schema.Get(keyword).Array() {
			if !schemaSupportsStrictOutput(child) {
				return false
			}
		}
	}
	return true
}
