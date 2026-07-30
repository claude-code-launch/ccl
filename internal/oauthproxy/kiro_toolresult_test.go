package oauthproxy

import (
	"encoding/json"
	"testing"
)

// kiroToolRoundTripRequest is the shape Claude Code sends after a tool ran: the
// assistant turn holds the tool_use, and the next user turn carries only the
// tool_result.
func kiroToolRoundTripRequest(t *testing.T, resultContent any) *kiroConvertedRequest {
	t.Helper()
	request := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 1024,
		"tools": []any{
			map[string]any{
				"name":         "Bash",
				"description":  "Run a shell command",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "run echo alive"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_01",
					"name":  "Bash",
					"input": map[string]any{"command": "echo alive"},
				},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_01",
					"content":     resultContent,
				},
			}},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := convertAnthropicToKiro(raw)
	if err != nil {
		t.Fatalf("convertAnthropicToKiro: %v", err)
	}
	return converted
}

// kiroCurrentToolResults extracts the tool results attached to the current turn.
func kiroCurrentToolResults(t *testing.T, converted *kiroConvertedRequest) []any {
	t.Helper()
	state, ok := converted.body["conversationState"].(map[string]any)
	if !ok {
		t.Fatalf("no conversationState: %#v", converted.body)
	}
	context := kiroCurrentMessageContext(state)
	if context == nil {
		t.Fatal("current message has no userInputMessageContext")
	}
	return kiroAnySlice(context["toolResults"])
}

func kiroToolResultText(t *testing.T, result any) string {
	t.Helper()
	blocks := kiroAnySlice(kiroAnyMap(result)["content"])
	if len(blocks) == 0 {
		return ""
	}
	return metadataString(kiroAnyMap(blocks[0]), "text")
}

func TestKiroToolResultReachesUpstream(t *testing.T) {
	converted := kiroToolRoundTripRequest(t, []any{
		map[string]any{"type": "text", "text": "alive"},
	})
	if converted.droppedToolRuns != 0 || converted.droppedToolUses != 0 {
		t.Fatalf("pairing dropped a paired tool call: uses=%d results=%d",
			converted.droppedToolUses, converted.droppedToolRuns)
	}
	results := kiroCurrentToolResults(t, converted)
	if len(results) != 1 {
		raw, _ := json.Marshal(converted.body)
		t.Fatalf("tool results = %d, want 1; body=%s", len(results), raw)
	}
	if got := kiroToolResultText(t, results[0]); got != "alive" {
		t.Fatalf("tool result text = %q, want %q", got, "alive")
	}
	if id := metadataString(kiroAnyMap(results[0]), "toolUseId"); id != "toolu_01" {
		t.Fatalf("toolUseId = %q", id)
	}
}

// A plain string tool_result is also legal in the Anthropic API.
func TestKiroToolResultAcceptsStringContent(t *testing.T) {
	converted := kiroToolRoundTripRequest(t, "alive")
	results := kiroCurrentToolResults(t, converted)
	if len(results) != 1 || kiroToolResultText(t, results[0]) != "alive" {
		raw, _ := json.Marshal(converted.body)
		t.Fatalf("string tool result was not forwarded: %s", raw)
	}
}

// A tool that produced no output must still be reported as having run, otherwise
// the model is left to guess whether the tool failed or returned nothing.
func TestKiroEmptyToolResultIsStillReported(t *testing.T) {
	for name, content := range map[string]any{
		"empty text block": []any{map[string]any{"type": "text", "text": ""}},
		"empty string":     "",
		"no content":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			converted := kiroToolRoundTripRequest(t, content)
			results := kiroCurrentToolResults(t, converted)
			if len(results) != 1 {
				raw, _ := json.Marshal(converted.body)
				t.Fatalf("empty tool output dropped the whole result: %s", raw)
			}
			if text := kiroToolResultText(t, results[0]); text == "" {
				t.Errorf("tool result text is empty; the model cannot tell an empty result from a lost one")
			}
		})
	}
}
