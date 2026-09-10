package oauthproxy

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStreamsRequireTerminalEvent(t *testing.T) {
	cases := []struct {
		name, partial, terminal string
		process                 func(io.Reader, *anthropicResponseAssembler) error
	}{
		{"responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", processCodexResponsesStream},
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", processChatCompletionsStream},
		{"gemini", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n", "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n", processGeminiStream},
		{"commandcode", "{\"type\":\"text-delta\",\"text\":\"partial\"}\n", "{\"type\":\"finish\",\"finishReason\":\"stop\"}\n", processCommandCodeStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []string{"", tc.partial} {
				a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
				err := tc.process(strings.NewReader(body), a)
				if !errors.Is(err, io.ErrUnexpectedEOF) || a.finished {
					t.Fatalf("truncated stream: err=%v finished=%v", err, a.finished)
				}
			}
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
			if err := tc.process(strings.NewReader(tc.partial+tc.terminal), a); err != nil || !a.finished {
				t.Fatalf("complete stream: err=%v finished=%v", err, a.finished)
			}
		})
	}
}

func TestResponsesDoesNotReplayOtherAdapterSignatures(t *testing.T) {
	for _, sig := range []string{"kiro", "gemini", "qoder", "ccl-openai-chat-signature-unavailable", "ccl-anthropic-signature-unavailable", "ccl-gemini-part-v1:abc"} {
		raw, _ := json.Marshal([]any{map[string]any{"type": "thinking", "thinking": "x", "signature": sig}, map[string]any{"type": "text", "text": "answer"}})
		items, err := codexMessageItems(anthropicMessage{Role: "assistant", Content: raw}, nil)
		if err != nil || len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
			t.Fatalf("foreign signature %q leaked: %v %v", sig, items, err)
		}
	}
	if !codexReplayableSignature("legacy+/=") {
		t.Fatal("legacy replay disabled")
	}
	if !codexReplayableSignature(codexReasoningSignaturePrefix + "broken") {
		t.Fatal("own malformed envelope must reach validation, not be silently dropped")
	}
}

func TestResponsesMultipleOutputMessagesAndParts(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"output":[{"id":"m1","type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":" extra"}]},{"id":"m2","type":"message","content":[{"type":"output_text","text":" second"}]}]}}`
	for _, prefix := range []string{
		"",
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"first\"}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"content_index\":0,\"delta\":\"first\"}\n\n",
	} {
		a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
		if err := processCodexResponsesStream(strings.NewReader(prefix+"data: "+terminal+"\n\n"), a); err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, b := range a.contentBlocks() {
			text.WriteString(b.Text)
		}
		if text.String() != "first extra second" {
			t.Fatalf("lost or duplicated text: %q", text.String())
		}
	}
}

func TestGeminiToolResultsResolveOpaqueIDs(t *testing.T) {
	for _, name := range []string{"lookup.v1", "lookup:v1", "lookup-v1", "lookup_v1"} {
		a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
		upstream, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"name": name, "args": map[string]any{}}}}}, "finishReason": "STOP"}}})
		if err := processGeminiNonStream(upstream, a); err != nil {
			t.Fatal(err)
		}
		blocks := a.contentBlocks()
		raw, _ := json.Marshal(map[string]any{"model": "gemini-test", "messages": []any{map[string]any{"role": "assistant", "content": blocks}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": blocks[0].ID, "content": "ok"}}}}})
		req, err := convertAnthropicToGemini(raw)
		if err != nil {
			t.Fatal(err)
		}
		call := gjson.GetBytes(req.geminiBody, "contents.0.parts.0.functionCall.name").String()
		result := gjson.GetBytes(req.geminiBody, "contents.1.parts.0.functionResponse.name").String()
		if call != name || result != name {
			t.Fatalf("name=%s call=%s result=%s", name, call, result)
		}
	}
}

func TestChatTemperaturePresence(t *testing.T) {
	for _, value := range []string{"", `,"temperature":0`, `,"temperature":0.7`} {
		req, err := convertAnthropicToChatCompletions([]byte(`{"model":"test","messages":[{"role":"user","content":"hello"}]` + value + `}`))
		if err != nil {
			t.Fatal(err)
		}
		got := gjson.GetBytes(req.body, "temperature")
		if got.Exists() != (value != "") {
			t.Fatalf("temperature presence changed: %s", req.body)
		}
		if value != "" && got.Float() != gjson.Get(`{`+strings.TrimPrefix(value, ",")+`}`, "temperature").Float() {
			t.Fatalf("temperature value changed: %s", req.body)
		}
	}
}
