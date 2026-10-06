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
