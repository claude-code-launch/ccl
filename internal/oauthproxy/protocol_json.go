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

// structuredOutputSchema renders a requested json_schema output in the shape
// both OpenAI dialects share (they only nest it differently). The schema is
// retained and strict mode downgraded for optional properties rather than
// silently making them required.
func structuredOutputSchema(format *conversationFormat) map[string]any {
	if format == nil {
		return nil
	}
	strict := format.Strict == nil || *format.Strict
	strict = strict && schemaSupportsStrictOutput(gjson.ParseBytes(format.Schema))
	result := map[string]any{"name": format.Name, "strict": strict, "schema": format.Schema}
	if format.Description != "" {
		result["description"] = format.Description
	}
	return result
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
