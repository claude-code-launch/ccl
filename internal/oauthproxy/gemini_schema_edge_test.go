package oauthproxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestSanitizeGeminiSchemaKeepsOriginalBytesOnUnusableInput pins the fallback:
// a schema the sanitizer cannot parse must still reach Gemini unchanged rather
// than being replaced by something the caller never wrote.
func TestSanitizeGeminiSchemaKeepsOriginalBytesOnUnusableInput(t *testing.T) {
	for _, raw := range []string{`not json`, `[1,2,3]`, `"a string"`, ``, `null`} {
		if out := string(sanitizeGeminiSchema([]byte(raw))); out != raw {
			t.Fatalf("sanitizeGeminiSchema(%q) = %q, want the input unchanged", raw, out)
		}
	}
	// A JSON object is always rewritten, even when it cleans to nothing.
	if out := string(sanitizeGeminiSchema([]byte(`{"additionalProperties":false}`))); out != `{"type":"object"}` {
		t.Fatalf("empty object cleaned to %q", out)
	}
}

// TestSanitizeGeminiSchemaRewritesOneOfAndTypeArrays covers the two rewrites
// Gemini's OpenAPI subset forces: oneOf is not accepted (anyOf is), and a type
// union has to become either a nullable single type or an anyOf list.
func TestSanitizeGeminiSchemaRewritesOneOfAndTypeArrays(t *testing.T) {
	raw := `{
		"type":"object",
		"properties":{
			"one":{"oneOf":[{"type":"string"},{"type":"integer"}]},
			"nullableOnly":{"type":["null"]},
			"union":{"type":["string","integer","null"]},
			"single":{"type":["BOOLEAN"]},
			"inferredObject":{"properties":{"a":{"type":"string"}}},
			"inferredArray":{"items":{"type":"string"}},
			"looseArray":{"type":"array"},
			"stringWithItems":{"type":"string","items":{"type":"string"}}
		}
	}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	if strings.Contains(text, "oneOf") {
		t.Fatalf("oneOf survived: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.one.anyOf.#").Int(); got != 2 {
		t.Fatalf("oneOf anyOf length = %d: %s", got, text)
	}
	if gjson.GetBytes(out, "properties.nullableOnly.type").Exists() ||
		!gjson.GetBytes(out, "properties.nullableOnly.nullable").Bool() {
		t.Fatalf("a null-only type must become nullable with no type: %s", text)
	}
	union := gjson.GetBytes(out, "properties.union")
	if union.Get("type").Exists() || union.Get("nullable").Bool() != true {
		t.Fatalf("union = %s", union.Raw)
	}
	if kinds := union.Get("anyOf.#.type").Array(); len(kinds) != 2 {
		t.Fatalf("union anyOf = %s", union.Get("anyOf").Raw)
	}
	if got := gjson.GetBytes(out, "properties.single.type").String(); got != "boolean" {
		t.Fatalf("type case not lowered: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.inferredObject.type").String(); got != "object" {
		t.Fatalf("object type not inferred: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.inferredArray.type").String(); got != "array" {
		t.Fatalf("array type not inferred: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.looseArray.items.type").String(); got != "string" {
		t.Fatalf("loose array items = %s", text)
	}
	if gjson.GetBytes(out, "properties.stringWithItems.items").Exists() {
		t.Fatalf("items survived on a non-array type: %s", text)
	}
}

// TestSanitizeGeminiSchemaNormalizesEnumsToStrings covers Gemini's string-only
// enum rule, including the null member that only makes the field nullable.
func TestSanitizeGeminiSchemaNormalizesEnumsToStrings(t *testing.T) {
	raw := `{"type":"object","properties":{
		"mixed":{"type":"integer","enum":[1,2.5,null,"three",true]},
		"numeric":{"enum":[7]},
		"empty":{"enum":[]}
	}}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	mixed := gjson.GetBytes(out, "properties.mixed")
	if mixed.Get("type").String() != "string" || !mixed.Get("nullable").Bool() {
		t.Fatalf("mixed enum = %s", mixed.Raw)
	}
	// The null member makes the field nullable instead of becoming an entry.
	if values := mixed.Get("enum").Array(); len(values) != 4 || values[0].String() != "1" ||
		values[1].String() != "2.5" || values[3].String() != "true" {
		t.Fatalf("mixed enum values = %s", mixed.Get("enum").Raw)
	}
	if got := gjson.GetBytes(out, "properties.numeric.enum.0").String(); got != "7" {
		t.Fatalf("numeric enum = %s, want the number stringified", text)
	}
	if gjson.GetBytes(out, "properties.empty.enum").Exists() {
		t.Fatalf("empty enum survived: %s", text)
	}
	// A const alongside enum must not be readded.
	if out := string(sanitizeGeminiSchema([]byte(`{"const":1,"enum":["a"]}`))); !strings.Contains(out, `"enum":["a"]`) {
		t.Fatalf("enum was overwritten by const: %s", out)
	}
	// A const alone becomes a one-value enum.
	if got := gjson.GetBytes(sanitizeGeminiSchema([]byte(`{"const":"x"}`)), "enum.0").String(); got != "x" {
		t.Fatalf("const = %q, want x", got)
	}
}

// TestSanitizeGeminiSchemaDropsFormatsGeminiRejects keeps only the format names
// Gemini's OpenAPI subset knows, per declared type.
func TestSanitizeGeminiSchemaDropsFormatsGeminiRejects(t *testing.T) {
	raw := `{"type":"object","properties":{
		"dateTime":{"type":"string","format":"date-time"},
		"enumFormat":{"type":"string","format":"enum"},
		"uri":{"type":"string","format":"uri"},
		"int32":{"type":"integer","format":"int32"},
		"int64":{"type":"integer","format":"int64"},
		"int16":{"type":"integer","format":"int16"},
		"double":{"type":"number","format":"double"},
		"doubleOnString":{"type":"string","format":"double"},
		"float":{"type":"number","format":"float"}
	}}`
	out := sanitizeGeminiSchema([]byte(raw))
	cases := map[string]bool{
		"dateTime": true, "enumFormat": true, "uri": false,
		"int32": true, "int64": true, "int16": false,
		"double": true, "doubleOnString": false, "float": true,
	}
	for name, want := range cases {
		if got := gjson.GetBytes(out, "properties."+name+".format").Exists(); got != want {
			t.Fatalf("%s format kept = %v, want %v: %s", name, got, want, out)
		}
	}
}

// TestSanitizeGeminiSchemaHandlesTupleItemsAndDefinitions covers the two
// remaining input shapes: a draft-07 tuple of item schemas, and the older
// "definitions" spelling of a definition table.
func TestSanitizeGeminiSchemaHandlesTupleItemsAndDefinitions(t *testing.T) {
	raw := `{
		"type":"object",
		"definitions":{"Pair":{"type":"object","properties":{"l":{"type":"string"}}}},
		"properties":{
			"tuple":{"type":"array","items":[{"type":"string"},{"type":"integer"}]},
			"emptyTuple":{"type":"array","items":[]},
			"scalarItems":{"type":"array","items":"nonsense"},
			"pair":{"$ref":"#/definitions/Pair"},
			"dangling":{"$ref":"#/definitions/Missing"},
			"full":{"$ref":"#/definitions/Missing","type":"string"}
		}
	}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	if got := gjson.GetBytes(out, "properties.tuple.items.type").String(); got != "string" {
		t.Fatalf("tuple items = %s", text)
	}
	if gjson.GetBytes(out, "properties.emptyTuple.items.type").Exists() {
		t.Fatalf("empty tuple items must be an anonymous schema: %s", text)
	}
	if gjson.GetBytes(out, "properties.scalarItems.items.type").Exists() {
		t.Fatalf("scalar items must be an anonymous schema: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.pair.properties.l.type").String(); got != "string" {
		t.Fatalf("definitions ref not inlined: %s", text)
	}
	if got := gjson.GetBytes(out, "properties.dangling.type").String(); got != "object" {
		t.Fatalf("dangling ref = %s", text)
	}
	if got := gjson.GetBytes(out, "properties.full.type").String(); got != "string" {
		t.Fatalf("a sibling type must survive a dangling ref, got %s", text)
	}
	if strings.Contains(text, "$ref") {
		t.Fatalf("a $ref leaked: %s", text)
	}
}

// TestSanitizeGeminiSchemaMergesAllOfMembers covers the allOf merge. Members
// union their properties, a later member refines the same property (the
// `allOf: [Base, {overrides}]` idiom), and a scalar keyword keeps the first
// value. A non-object member is skipped.
func TestSanitizeGeminiSchemaMergesAllOfMembers(t *testing.T) {
	raw := `{"allOf":[
		{"type":"object","properties":{"a":{"type":"string"},"shared":{"type":"string"}},"required":["b","a"],"description":"first"},
		{"properties":{"b":{"type":"integer"},"shared":{"type":"integer"}},"required":["b","undeclared"],"description":"second"},
		"not-an-object"
	]}`
	out := sanitizeGeminiSchema([]byte(raw))
	text := string(out)
	if gjson.GetBytes(out, "properties.a.type").String() != "string" ||
		gjson.GetBytes(out, "properties.b.type").String() != "integer" {
		t.Fatalf("allOf properties = %s", text)
	}
	if got := gjson.GetBytes(out, "properties.shared.type").String(); got != "integer" {
		t.Fatalf("shared = %q, want the later member to refine it", got)
	}
	// "undeclared" names no property anywhere in the merged object, so Gemini
	// would reject it; the union keeps only names that are declared.
	if got := gjson.GetBytes(out, "required").Raw; got != `["a","b"]` {
		t.Fatalf("allOf required = %s, want a sorted, declared-only union", got)
	}
	if got := gjson.GetBytes(out, "description").String(); got != "first" {
		t.Fatalf("allOf description = %q, want the first member to win", got)
	}
}

// TestSanitizeGeminiSchemaBoundsRecursion keeps a pathological schema from
// recursing without bound.
func TestSanitizeGeminiSchemaBoundsRecursion(t *testing.T) {
	deep := `{"type":"object","properties":{"leaf":{"type":"string"}}}`
	for range geminiSchemaMaxDepth + 4 {
		deep = `{"type":"object","properties":{"child":` + deep + `}}`
	}
	out := sanitizeGeminiSchema([]byte(deep))
	if len(out) == 0 {
		t.Fatal("deep schema produced no output")
	}
	// Walking past the limit yields a bare object rather than disappearing.
	depth := 0
	node := gjson.GetBytes(out, "properties.child")
	for node.Exists() {
		depth++
		node = node.Get("properties.child")
	}
	if depth == 0 || depth > geminiSchemaMaxDepth+2 {
		t.Fatalf("walked %d levels of a schema bounded at %d", depth, geminiSchemaMaxDepth)
	}
}

// TestUnionStringAnySortsAndDeduplicates pins the helper's contract, which the
// allOf merge depends on for stable output.
func TestUnionStringAnySortsAndDeduplicates(t *testing.T) {
	got := unionStringAny([]any{"b", "a", 7, nil}, []any{"a", "c"})
	want := []any{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unionStringAny = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("unionStringAny = %v, want %v", got, want)
		}
	}
	if empty := unionStringAny(nil, nil); len(empty) != 0 {
		t.Fatalf("unionStringAny(nil, nil) = %v", empty)
	}
}
