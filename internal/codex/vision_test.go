package codex

import (
	"encoding/json"
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestCodexCatalogKeepsAdvertisedImageCapabilities(t *testing.T) {
	for _, modalities := range [][]string{nil, {"text"}, {"text", "image"}} {
		model := newModel("alias", "model", map[string]protocol.ModelInfo{"model": {InputModalities: modalities}}, nil)
		got := model["input_modalities"].([]string)
		wantImage := len(modalities) == 2
		if (len(got) == 2) != wantImage || got[0] != "text" || model["node_repl_disabled"] != false {
			t.Fatalf("advertised=%v model=%+v", modalities, model)
		}
	}
}

func TestCodexToolScreenshotsReachMessagesAsImages(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		body, _, err := toMessages(responseRequest{}, []map[string]any{{"type": kind, "call_id": "screen_call", "output": []any{
			map[string]any{"type": "input_text", "text": "screen-fixture"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
		}}}, Route{Model: "model"})
		if err != nil {
			t.Fatal(err)
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		tool := request["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		content, ok := tool["content"].([]any)
		if !ok || len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
			t.Fatalf("screenshot was serialized as text: %s", body)
		}
		source := content[1].(map[string]any)["source"].(map[string]any)
		if source["media_type"] != "image/png" || source["data"] != "aGVsbG8=" {
			t.Fatalf("screenshot changed: %+v", source)
		}
	}
}
