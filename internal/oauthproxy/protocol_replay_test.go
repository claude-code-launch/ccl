package oauthproxy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiSignedPartsRoundTrip(t *testing.T) {
	parts := []any{
		map[string]any{"text": "thought", "thought": true, "thoughtSignature": "thought+/="},
		map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{"q": "x"}}, "thoughtSignature": "tool+/="},
		map[string]any{"text": "answer", "thoughtSignature": "text+/="},
		map[string]any{"text": "", "thoughtSignature": "empty+/="},
	}
	for _, streaming := range []bool{false, true} {
		for _, splitMessages := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/split=%t", streaming, splitMessages), func(t *testing.T) {
				response := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": parts}, "finishReason": "STOP"}}}
				raw, _ := json.Marshal(response)
				writer := httptest.NewRecorder()
				a := newAnthropicResponseAssembler(&anthropicAdapterRequest{stream: streaming}, writer)
				var err error
				if streaming {
					err = processGeminiStream(strings.NewReader("data: "+string(raw)+"\n\n"), a)
				} else {
					err = processGeminiNonStream(raw, a)
				}
				if err != nil {
					t.Fatal(err)
				}
				if streaming && strings.Count(writer.Body.String(), geminiPartSignaturePrefix) != len(parts) {
					t.Fatal("missing SSE signatures")
				}
				messages := []any{map[string]any{"role": "user", "content": "hello"}}
				blocks := a.contentBlocks()
				if splitMessages {
					for _, block := range blocks {
						messages = append(messages, map[string]any{"role": "assistant", "content": []any{block}})
					}
				} else {
					messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
				}
				var toolID string
				for _, block := range blocks {
					if block.Type == "tool_use" {
						toolID = block.ID
					}
				}
				messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": "found"}}})
				request, _ := json.Marshal(map[string]any{"model": "gemini-test", "messages": messages})
				converted, err := convertAnthropicToGemini(request)
				if err != nil {
					t.Fatal(err)
				}
				var replay []any
				for _, content := range gjson.GetBytes(converted.geminiBody, "contents").Array() {
					if content.Get("role").String() != "model" {
						continue
					}
					for _, part := range content.Get("parts").Array() {
						var value any
						if err := json.Unmarshal([]byte(part.Raw), &value); err != nil {
							t.Fatal(err)
						}
						replay = append(replay, value)
					}
				}
				if !reflect.DeepEqual(parts, replay) {
					t.Fatalf("native parts changed: got %#v want %#v", replay, parts)
				}
			})
		}
	}
}

func TestGeminiStringMessagesAreObjectParts(t *testing.T) {
	for _, role := range []string{"user", "assistant", "system"} {
		raw, _ := json.Marshal(map[string]any{"model": "gemini-test", "messages": []any{map[string]any{"role": role, "content": "hello\n\"world\""}}})
		converted, err := convertAnthropicToGemini(raw)
		if err != nil {
			t.Fatal(err)
		}
		part := gjson.GetBytes(converted.geminiBody, "contents.0.parts.0")
		if !part.IsObject() || part.Get("text").String() != "hello\n\"world\"" {
			t.Fatalf("invalid %s part: %s", role, part.Raw)
		}
	}
}

func TestProtocolStreamErrorsDoNotFinishSuccessfully(t *testing.T) {
	for _, protocol := range []string{"chat", "gemini", "commandcode"} {
		t.Run(protocol, func(t *testing.T) {
			a := newAnthropicResponseAssembler(&anthropicAdapterRequest{}, nil)
			var err error
			switch protocol {
			case "chat":
				err = processChatCompletionsStream(strings.NewReader("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"), a)
			case "gemini":
				err = processGeminiStream(strings.NewReader("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"), a)
			case "commandcode":
				err = processCommandCodeStream(strings.NewReader("{\"type\":\"error\",\"errorText\":\"upstream failed\"}\n"), a)
			}
			if err == nil || !strings.Contains(err.Error(), "upstream failed") || a.finished {
				t.Fatalf("error=%v finished=%v", err, a.finished)
			}
		})
	}
}
