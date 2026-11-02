package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestCodexCatalogPublishesReasoningControls(t *testing.T) {
	info := protocol.ModelInfo{ID: "reasoner", ReasoningLevels: protocol.ChatReasoningLevels(), DefaultReasoningLevel: "medium"}
	model := newModel("ccl/work/reasoner", "reasoner", map[string]protocol.ModelInfo{"reasoner": info}, nil)
	levels, ok := model["supported_reasoning_levels"].([]protocol.ReasoningLevel)
	if !ok || len(levels) != 3 || levels[0].Effort != "low" || levels[2].Effort != "high" || model["default_reasoning_level"] != "medium" {
		t.Fatalf("missing reasoning picker controls: %+v", model)
	}
	unknown := newModel("ccl/work/plain", "plain", nil, nil)
	if levels := unknown["supported_reasoning_levels"].([]protocol.ReasoningLevel); len(levels) != 0 || unknown["default_reasoning_level"] != nil {
		t.Fatal("reasoning support was invented for an unknown model")
	}
	info.ReasoningLevels = []protocol.ReasoningLevel{{Effort: "high"}, {Effort: "invalid"}, {Effort: "high"}, {Effort: "xhigh"}}
	levels, defaultLevel := reasoningLevels(info)
	if len(levels) != 2 || defaultLevel != "high" {
		t.Fatal("invalid/duplicate effort was advertised or default was outside the list")
	}
}

func TestCodexReasoningSurvivesChatAndStructuredOutputConversion(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high"} {
		request := responseRequest{Model: "alias", Input: json.RawMessage(`"fixture"`), Reasoning: map[string]any{"effort": effort}, Text: map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}}
		items, _ := inputItems(request.Input)
		raw, _, err := toMessages(request, items, Route{Model: "actual"})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal("invalid Messages JSON")
		}
		thinking, _ := body["thinking"].(map[string]any)
		output, _ := body["output_config"].(map[string]any)
		if thinking["type"] != "adaptive" || output["effort"] != effort || output["format"] == nil {
			t.Fatalf("reasoning/format lost for %s: %s", effort, raw)
		}
	}
}

func TestCodexAnthropicReasoningUsesValidTokenBudget(t *testing.T) {
	request := responseRequest{Input: json.RawMessage(`"fixture"`), MaxOutputTokens: 1024, Reasoning: map[string]any{"effort": "high"}}
	items, _ := inputItems(request.Input)
	raw, _, err := toMessages(request, items, Route{Model: "actual", ThinkingBudget: true})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || number(body["max_tokens"]) <= number(thinking["budget_tokens"]) {
		t.Fatalf("invalid Anthropic thinking budget: %s", raw)
	}
}

func TestCodexDuplicateNamesKeepDistinctModelVariants(t *testing.T) {
	models := []Model{{"slug": "ccl/work/hy4", "display_name": "Hy4 preview"}, {"slug": "ccl/work/hy4-x", "display_name": "Hy4 preview"}, {"slug": "ccl/work/other", "display_name": "Other"}}
	routes := map[string]Route{"ccl/work/hy4": {Model: "hy4"}, "ccl/work/hy4-x": {Model: "hy4-x"}}
	disambiguateModelNames(models, routes)
	if models[0]["display_name"] == models[1]["display_name"] || !strings.Contains(models[1]["display_name"].(string), "[hy4-x]") || models[2]["display_name"] != "Other" {
		t.Fatal("model variants are still ambiguous or unique names changed")
	}
}

func TestCodexWorkBuddyPickerMergesRevisionsWithoutDroppingRoutes(t *testing.T) {
	models := []Model{{"slug": "ccl/work/hy4-preview-x", "display_name": "Hy4 preview"}, {"slug": "ccl/work/hy4-preview", "display_name": "Hy4 preview"}, {"slug": "ccl/work/other", "display_name": "Other"}}
	routes := map[string]Route{"ccl/work/hy4-preview-x": {Model: "hy4-preview-x"}, "ccl/work/hy4-preview": {Model: "hy4-preview"}, "ccl/work/other": {Model: "other"}}
	picker := uniqueWorkBuddyModels(models, routes)
	if len(picker) != 2 || picker[0]["slug"] != "ccl/work/hy4-preview" || picker[0]["display_name"] != "Hy4 preview" {
		t.Fatalf("internal revisions still duplicate the picker: %v", picker)
	}
	if len(routes) != 3 || routes["ccl/work/hy4-preview-x"].Model != "hy4-preview-x" {
		t.Fatal("resumed sessions lost their original route")
	}
}

func TestCodexLabelsOmitConfigurationNames(t *testing.T) {
	model := newModel("ccl/private-config/reasoner", "reasoner", map[string]protocol.ModelInfo{"reasoner": {DisplayName: "Reasoner"}}, nil)
	if model["display_name"] != "Reasoner" || strings.Contains(model["description"].(string), "private-config") {
		t.Fatal("configuration name leaked into a model label")
	}
	merged, err := MergeCatalog(Catalog{Models: []Model{{"slug": "native", "display_name": "Reasoner"}}}, []Model{model})
	if err != nil || merged.Models[0]["display_name"] != "Reasoner (CCL)" || model["display_name"] != "Reasoner" {
		t.Fatalf("native collision changed the source or needs a shorter label: %v %v", merged, err)
	}
	merged, err = MergeCatalog(Catalog{Models: []Model{{"slug": "native", "display_name": "Native"}, {"slug": "ccl/old/reasoner", "display_name": "Reasoner"}}}, []Model{model})
	if err != nil || len(merged.Models) != 2 || merged.Models[0]["display_name"] != "Reasoner" {
		t.Fatalf("stale CCL cache caused a false name collision: %v %v", merged, err)
	}
}
