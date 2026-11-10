package oauthproxy

import (
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
		output := []conversationPart{{Kind: partImage, MediaType: "image/png", Data: "aGVsbG8="}}
		if withText {
			output = append(output, conversationPart{Kind: partText, Text: "screen-text"})
		}
		turnParts := []conversationPart{
			{Kind: partToolResult, CallID: "screen", Output: output},
			{Kind: partToolResult, CallID: "other", Output: []conversationPart{{Kind: partText, Text: "other-result"}}},
		}
		messages, _ := qoderTransformUserTurn(conversationTurn{Role: "user", Parts: turnParts})
		if len(messages) != 3 {
			t.Fatalf("messages=%+v", messages)
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
