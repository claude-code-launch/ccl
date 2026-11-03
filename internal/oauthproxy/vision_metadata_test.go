package oauthproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderImageCapabilitiesReachRuntimeMetadata(t *testing.T) {
	info, ok := qoderModelDetail(qoderModel{ID: "vision-only", Vision: true})
	if !ok || strings.Join(info.InputModalities, ",") != "text,image" {
		t.Fatalf("Qoder discarded is_vl: %+v", info)
	}
	_, _, _, details := buildWorkBuddyRoutes("", []workbuddyModel{{ID: "vision", SupportsImages: true}, {ID: "plain"}})
	if strings.Join(details["vision"].InputModalities, ",") != "text,image" || len(details["plain"].InputModalities) != 0 {
		t.Fatalf("WorkBuddy discarded supportsImages or invented vision: %+v", details)
	}
	runtime := &Runtime{modelDetails: details}
	copy := runtime.ModelDetails()
	copy["vision"].InputModalities[1] = "modified"
	if runtime.ModelDetails()["vision"].InputModalities[1] != "image" {
		t.Fatal("caller changed runtime image capabilities")
	}
}

func TestQoderPreservesToolScreenshotsAfterAllToolReplies(t *testing.T) {
	for _, withText := range []bool{false, true} {
		content := []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}}
		if withText {
			content = append(content, map[string]any{"type": "text", "text": "screen-text"})
		}
		raw, _ := json.Marshal([]any{
			map[string]any{"type": "tool_result", "tool_use_id": "screen", "content": content},
			map[string]any{"type": "tool_result", "tool_use_id": "other", "content": "other-result"},
		})
		messages, _, err := qoderTransformUserMessage(raw)
		if err != nil || len(messages) != 3 {
			t.Fatalf("messages=%+v err=%v", messages, err)
		}
		if messages[0].(map[string]any)["role"] != "tool" || messages[1].(map[string]any)["role"] != "tool" || messages[2].(map[string]any)["role"] != "user" {
			t.Fatalf("image interrupted tool replies: %+v", messages)
		}
		if strings.Contains(messages[0].(map[string]any)["content"].(string), "aGVsbG8=") {
			t.Fatal("base64 screenshot duplicated into tool text")
		}
		parts := messages[2].(map[string]any)["content"].([]any)
		url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
		if url != "data:image/png;base64,aGVsbG8=" {
			t.Fatalf("screenshot lost: %+v", messages)
		}
	}
}
