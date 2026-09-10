package oauthproxy

import (
	"strings"
	"testing"
)

func TestRefusalContentAndStopReason(t *testing.T) {
	for _, kind := range []string{"chat-stream", "chat-json", "responses-stream", "responses-json"} {
		t.Run(kind, func(t *testing.T) {
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
			var err error
			switch kind {
			case "chat-stream":
				err = processChatCompletionsStream(strings.NewReader("data: {\"choices\":[{\"delta\":{\"refusal\":\"Cannot help.\"},\"finish_reason\":\"stop\"}]}\n\n"), a)
			case "chat-json":
				err = processChatCompletionsNonStream([]byte(`{"choices":[{"message":{"content":null,"refusal":"Cannot help."},"finish_reason":"stop"}]}`), a)
			case "responses-stream":
				err = processCodexResponsesStream(strings.NewReader("data: {\"type\":\"response.refusal.delta\",\"output_index\":0,\"delta\":\"Cannot help.\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"Cannot help.\"}]}]}}\n\n"), a)
			case "responses-json":
				err = processCodexResponsesStream(strings.NewReader(`{"output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot help."}]}]}`), a)
			}
			if err != nil {
				t.Fatal(err)
			}
			blocks := a.contentBlocks()
			if len(blocks) != 1 || blocks[0].Text != "Cannot help." || a.resolvedStopReason() != "refusal" {
				t.Fatalf("blocks=%+v stop=%s", blocks, a.resolvedStopReason())
			}
		})
	}
}

func TestGeminiToolIDsUniqueAcrossResponses(t *testing.T) {
	seen := make(map[string]bool)
	for range 3 {
		a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
		if err := processGeminiNonStream([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}`), a); err != nil {
			t.Fatal(err)
		}
		id := a.contentBlocks()[0].ID
		if id == "" || seen[id] {
			t.Fatalf("duplicate tool ID: %s", id)
		}
		if toolNameFromClaudeToolUseID(id) != "lookup" {
			t.Fatalf("legacy name fallback broken: %s", id)
		}
		seen[id] = true
	}
}
