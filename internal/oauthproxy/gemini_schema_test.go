package oauthproxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeGeminiSchemaStripsUnsupportedKeywords(t *testing.T) {
	raw := `{
		"$schema":"http://json-schema.org/draft-07/schema#",
		"type":"object",
		"additionalProperties":false,
		"$defs":{"Opt":{"type":"object","properties":{"n":{"type":"integer","exclusiveMinimum":0}}}},
		"properties":{
			"env":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{"type":"string"}},
			"name":{"type":["string","null"],"default":"x","format":"uri"},
			"mode":{"const":"fast"},
			"opt":{"$ref":"#/$defs/Opt"},
			"list":{"type":"array","items":{"type":"string","pattern":"^a"}},
			"ghost":{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a","zzz"]}]}
		},
		"required":["name","missing"]
	}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	for _, bad := range []string{"$schema", "propertyNames", "additionalProperties", "$defs", "$ref", "exclusiveMinimum", "\"default\"", "\"const\"", "\"pattern\"", "\"uri\"", "allOf"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%s leaked: %s", bad, text)
		}
	}
	if got := gjson.GetBytes(out, "properties.name.type").String(); got != "string" {
		t.Fatalf("name type = %q: %s", got, text)
	}
	if !gjson.GetBytes(out, "properties.name.nullable").Bool() {
		t.Fatalf("name not nullable: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.mode.enum.0").String(); got != "fast" {
		t.Fatalf("const not converted: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.opt.properties.n.type").String(); got != "integer" {
		t.Fatalf("ref not inlined: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.ghost.properties.a.type").String(); got != "string" {
		t.Fatalf("allOf not merged: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.ghost.required").Raw; got != `["a"]` {
		t.Fatalf("ghost required = %s", got)
	}
	if got := gjson.GetBytes(out, "required").Raw; got != `["name"]` {
		t.Fatalf("required = %s", got)
	}
	if gjson.GetBytes(out, "properties.env.properties").Exists() {
		t.Fatalf("empty properties kept: %s", text)
	}
}

func TestSanitizeGeminiSchemaSelfReferenceTerminates(t *testing.T) {
	raw := `{"type":"object","$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}},"properties":{"root":{"$ref":"#/$defs/Node"}}}`
	out := sanitizeGeminiSchema([]byte(raw))
	if !gjson.GetBytes(out, "properties.root.properties.next").Exists() {
		t.Fatalf("unexpected: %s", out)
	}
}

func TestSanitizeGeminiSchemaCollapsesNullableAnyOfUnion(t *testing.T) {
	raw := `{
		"type":"object",
		"properties":{
			"path":{"anyOf":[{"type":"string"},{"type":"null"}],"description":"Optional file path"},
			"multi":{"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]}
		}
	}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	if strings.Contains(text, `"null"`) {
		t.Fatalf("raw null type leaked into Gemini schema: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.path.type").String(); got != "string" {
		t.Fatalf("path.type = %q, want string: %s", got, text)
	}
	if !gjson.GetBytes(out, "properties.path.nullable").Bool() {
		t.Fatalf("path.nullable = false, want true: %s", text)
	}
	if gjson.GetBytes(out, "properties.path.anyOf").Exists() {
		t.Fatalf("single non-null anyOf branch should collapse: %s", text)
	}
	if !gjson.GetBytes(out, "properties.multi.nullable").Bool() {
		t.Fatalf("multi.nullable = false, want true: %s", text)
	}
	if got := len(gjson.GetBytes(out, "properties.multi.anyOf").Array()); got != 2 {
		t.Fatalf("multi.anyOf len = %d, want 2: %s", got, text)
	}
}
