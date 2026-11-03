package oauthproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestQoderReasoningLevelsFollowCatalogRungs(t *testing.T) {
	kimi := map[string]any{
		"is_reasoning": false,
		"thinking_config": map[string]any{
			"enabled": map[string]any{
				"efforts": map[string]any{
					"max":  map[string]any{"is_default": true},
					"low":  map[string]any{},
					"high": map[string]any{},
				},
			},
		},
	}
	levels, chosen := qoderReasoningLevels(kimi)
	if chosen != "max" || effortNames(levels) != "low,high,max" {
		t.Fatalf("kimi levels = %s default %q", effortNames(levels), chosen)
	}
	if levels[0].Description == "" || levels[2].Description == "" {
		t.Fatalf("missing effort descriptions: %+v", levels)
	}

	qwen := map[string]any{
		"thinking_config": map[string]any{
			"enabled": map[string]any{
				"efforts": map[string]any{
					"medium": map[string]any{},
					"low":    map[string]any{},
					"xhigh":  map[string]any{"is_default": true, "description": "  Extreme thinking intensity  "},
				},
			},
		},
	}
	levels, chosen = qoderReasoningLevels(qwen)
	if chosen != "xhigh" || effortNames(levels) != "low,medium,xhigh" || levels[2].Description != "Extreme thinking intensity" {
		t.Fatalf("qwen levels = %s default %q desc %q", effortNames(levels), chosen, levels[2].Description)
	}

	toggleOnly := map[string]any{
		"is_reasoning": false,
		"thinking_config": map[string]any{
			"disabled": map[string]any{},
			"enabled":  map[string]any{"description": "thinking", "is_default": true},
		},
	}
	if levels, chosen = qoderReasoningLevels(toggleOnly); levels != nil || chosen != "" {
		t.Fatalf("thinking toggle invented a slider: %+v %q", levels, chosen)
	}
	if levels, chosen = qoderReasoningLevels(nil); levels != nil || chosen != "" {
		t.Fatalf("nil config invented a slider: %+v %q", levels, chosen)
	}
}

func TestQoderCatalogJSONPublishesEffortControlsWithoutAPrice(t *testing.T) {
	raw := []byte(`{"chat":[
		{"key":"kmodel","enable":true,"display_name":"Kimi","is_reasoning":false,
		 "thinking_config":{"enabled":{"efforts":{"low":{},"high":{},"max":{"is_default":true}}}}},
		{"key":"qmodel","enable":true,"display_name":"Qwen Plus","is_reasoning":false,"price_factor":0.5,
		 "thinking_config":{"enabled":{"description":"toggle"},"disabled":{"description":"off"}}},
		{"key":"auto","enable":true,"display_name":"Auto","is_reasoning":false}
	]}`)
	var catalog struct {
		Chat []map[string]any `json:"chat"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	models := qoderModelsFromCatalog(catalog.Chat)
	details := map[string]protocol.ModelInfo{}
	for _, model := range models {
		info, ok := qoderModelDetail(model)
		if !ok {
			continue
		}
		details[info.ID] = info
	}
	kimi := details["kmodel"]
	if kimi.DefaultReasoningLevel != "max" || effortNames(kimi.ReasoningLevels) != "low,high,max" || kimi.RateMultiplier != nil {
		t.Fatalf("kimi detail = %+v", kimi)
	}
	qwen := details["qmodel"]
	if len(qwen.ReasoningLevels) != 0 || qwen.RateMultiplier == nil || *qwen.RateMultiplier != 0.5 {
		t.Fatalf("toggle-only model = %+v", qwen)
	}
	if _, published := details["auto"]; published {
		t.Fatal("a model with no rate and no rungs was published")
	}
}

func TestQoderModelDetailKeepsCatalogContextWindow(t *testing.T) {
	raw := []byte(`{"chat":[
		{"key":"qmodel_38max","enable":true,"display_name":"Qwen3.8-Max","max_input_tokens":180000,"max_output_tokens":32768,
		 "context_config":{"200k":{"token_count":200000},"1m":{"token_count":1000000}},
		 "thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{"is_default":true}}}}},
		{"key":"auto","enable":true,"display_name":"Auto","max_input_tokens":180000},
		{"key":"blank","enable":true,"display_name":"Blank"}
	]}`)
	var catalog struct {
		Chat []map[string]any `json:"chat"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	details := map[string]protocol.ModelInfo{}
	for _, model := range qoderModelsFromCatalog(catalog.Chat) {
		info, ok := qoderModelDetail(model)
		if !ok {
			continue
		}
		details[info.ID] = info
	}
	qwen := details["qmodel_38max"]
	if qwen.ContextWindow != 1_000_000 || qwen.MaxOutputTokens != 32768 || qwen.DefaultReasoningLevel != "xhigh" {
		t.Fatalf("qwen detail = %+v", qwen)
	}
	if details["auto"].ContextWindow != 180_000 || len(details["auto"].ReasoningLevels) != 0 {
		t.Fatalf("context-only model = %+v", details["auto"])
	}
	if _, published := details["blank"]; published {
		t.Fatal("a model with no window, rate, or rungs was published")
	}
}

func TestQoderMatchEffortUsesOnlyAdvertisedRungs(t *testing.T) {
	qwen, _ := qoderReasoningLevels(map[string]any{
		"thinking_config": map[string]any{"enabled": map[string]any{"efforts": map[string]any{
			"low": map[string]any{}, "medium": map[string]any{}, "xhigh": map[string]any{"is_default": true},
		}}},
	})
	kimi, _ := qoderReasoningLevels(map[string]any{
		"thinking_config": map[string]any{"enabled": map[string]any{"efforts": map[string]any{
			"low": map[string]any{}, "high": map[string]any{}, "max": map[string]any{"is_default": true},
		}}},
	})
	cases := []struct {
		name   string
		levels []protocol.ReasoningLevel
		asked  string
		want   string
	}{
		{"exact", kimi, "high", "high"},
		{"qwen high", qwen, "high", "xhigh"},
		{"qwen max", qwen, "MAX", "xhigh"},
		{"minimal", qwen, "minimal", "low"},
		{"ultra omitted", qwen, "ultra", ""},
		{"off omitted", kimi, "off", ""},
		{"empty omitted", kimi, "", ""},
	}
	for _, tc := range cases {
		if got := qoderMatchEffort(tc.levels, tc.asked); got != tc.want {
			t.Fatalf("%s: match %q = %q, want %q", tc.name, tc.asked, got, tc.want)
		}
	}
}

func TestQoderRequestCarriesMatchedEffort(t *testing.T) {
	catalog := []qoderModel{{
		ID: "qmodel_38max", Name: "Qwen3.8-Max", Reasoning: true, Source: "system",
		Config: map[string]any{"thinking_config": map[string]any{"enabled": map[string]any{"efforts": map[string]any{
			"low": map[string]any{}, "medium": map[string]any{}, "xhigh": map[string]any{"is_default": true},
		}}}},
	}, {
		ID: "auto", Name: "Auto", Source: "system", Config: map[string]any{},
	}}
	parameters := qoderParameters(t, `{
		"model":"qmodel_38max","max_tokens":100,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"messages":[{"role":"user","content":"hi"}]
	}`, catalog)
	if parameters["enable_thinking"] != true || parameters["reasoning_effort"] != "xhigh" || parameters["max_tokens"] != 100 {
		t.Fatalf("matched effort parameters = %+v", parameters)
	}

	parameters = qoderParameters(t, `{
		"model":"qmodel_38max","max_tokens":100,
		"thinking":{"type":"disabled"},
		"output_config":{"effort":"low"},
		"messages":[{"role":"user","content":"hi"}]
	}`, catalog)
	if _, ok := parameters["enable_thinking"]; ok || parameters["reasoning_effort"] != nil {
		t.Fatalf("disabled thinking still set effort: %+v", parameters)
	}

	parameters = qoderParameters(t, `{
		"model":"qmodel_38max","max_tokens":100,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"ultra"},
		"messages":[{"role":"user","content":"hi"}]
	}`, catalog)
	if _, ok := parameters["enable_thinking"]; ok || parameters["reasoning_effort"] != nil {
		t.Fatalf("unlisted effort was sent: %+v", parameters)
	}

	parameters = qoderParameters(t, `{
		"model":"auto","max_tokens":100,
		"output_config":{"effort":"high"},
		"messages":[{"role":"user","content":"hi"}]
	}`, catalog)
	if _, ok := parameters["enable_thinking"]; ok || parameters["reasoning_effort"] != nil || parameters["max_tokens"] != 100 {
		t.Fatalf("model without rungs gained effort parameters: %+v", parameters)
	}
}

func qoderParameters(t *testing.T, raw string, catalog []qoderModel) map[string]any {
	t.Helper()
	converted, err := convertAnthropicToQoder([]byte(raw), catalog)
	if err != nil {
		t.Fatal(err)
	}
	parameters, _ := converted.body["parameters"].(map[string]any)
	return parameters
}

func effortNames(levels []protocol.ReasoningLevel) string {
	names := make([]string, 0, len(levels))
	for _, level := range levels {
		names = append(names, level.Effort)
	}
	return strings.Join(names, ",")
}
