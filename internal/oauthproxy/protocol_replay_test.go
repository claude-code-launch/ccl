package oauthproxy

import (
	"encoding/base64"
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
				a := newResponseAssembler(&adapterRequest{stream: streaming}, writer)
				var err error
				if streaming {
					err = processGeminiStream(strings.NewReader("data: "+string(raw)+"\n\n"), a)
				} else {
					err = processGeminiNonStream(raw, a)
				}
				if err != nil {
					t.Fatal(err)
				}
				if streaming && strings.Count(writer.Body.String(), geminiPartSignaturePrefix) < len(parts) {
					t.Fatal("missing SSE signatures")
				}
				input := []any{map[string]any{"role": "user", "content": "hello"}}
				for _, item := range a.outputItems() {
					input = append(input, item)
				}
				var toolID string
				for _, block := range a.contentBlocks() {
					if block.Type == "tool_use" {
						toolID = block.ID
					}
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": toolID, "output": "found"})
				request, _ := json.Marshal(map[string]any{
					"model": "gemini-test",
					"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
					"input": input,
				})
				converted, err := convertResponsesToGemini(request)
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

func TestGemini3UnsignedFunctionCallGetsBypassSignature(t *testing.T) {
	tool := func(id, cmd string) map[string]any {
		return map[string]any{"type": "function_call", "call_id": id, "name": "exec", "arguments": fmt.Sprintf(`{"cmd":%q}`, cmd)}
	}
	result := func(id, text string) map[string]any {
		return map[string]any{"type": "function_call_output", "call_id": id, "output": text}
	}
	input := []any{
		map[string]any{"role": "user", "content": "run it"},
		map[string]any{"role": "assistant", "content": "checking"},
		tool("call_1", "ls"),
		tool("call_2", "pwd"),
		result("call_1", "a"),
		result("call_2", "b"),
		tool("call_3", "echo"),
		result("call_3", "ok"),
	}
	raw, _ := json.Marshal(map[string]any{
		"model": "gemini-3-flash", "input": input,
		"tools": []any{map[string]any{"type": "function", "name": "exec", "description": "run", "parameters": map[string]any{"type": "object"}}},
	})
	converted, err := convertResponsesToGemini(raw)
	if err != nil {
		t.Fatal(err)
	}
	var steps []gjson.Result
	for _, content := range gjson.GetBytes(converted.geminiBody, "contents").Array() {
		if content.Get("role").String() == "model" {
			steps = append(steps, content)
		}
	}
	if len(steps) != 2 {
		t.Fatalf("model steps = %d", len(steps))
	}
	first, second := steps[0].Get("parts"), steps[1].Get("parts")
	if first.Get("0.thoughtSignature").Exists() {
		t.Fatalf("text part gained a signature: %s", first.Get("0").Raw)
	}
	if first.Get("1.functionCall.name").String() != "exec" || first.Get("1.thoughtSignature").String() != geminiBypassThoughtSignature {
		t.Fatalf("first call = %s", first.Raw)
	}
	if first.Get("2.functionCall.name").String() != "exec" || first.Get("2.thoughtSignature").String() != geminiBypassThoughtSignature {
		t.Fatalf("parallel call at position 2 = %s", first.Get("2").Raw)
	}
	if second.Get("0.thoughtSignature").String() != geminiBypassThoughtSignature {
		t.Fatalf("next step = %s", second.Raw)
	}

	claudeRaw, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "input": input, "tools": []any{map[string]any{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object"}}}})
	claude, err := convertResponsesToAntigravity(claudeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(claude.geminiBody, "contents.1.parts.1.thoughtSignature").Exists() {
		t.Fatal("Claude function calls must not gain a Gemini thought signature")
	}
	if gjson.GetBytes(claude.geminiBody, "contents.1.parts.2.thoughtSignature").Exists() {
		t.Fatal("Claude parallel function calls must not gain a Gemini thought signature")
	}
}

func TestGemini3RealSignatureStaysWhileLaterCallIsSigned(t *testing.T) {
	rawPart := `{"functionCall":{"name":"exec","args":{"cmd":"ls"}},"thoughtSignature":"opaque+/="}`
	signature := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(rawPart))
	input := []any{
		map[string]any{"role": "user", "content": "run it"},
		map[string]any{"id": "rs_ccl_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "check"}}, "encrypted_content": signature},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": `{"cmd":"ls"}`},
		map[string]any{"type": "function_call", "call_id": "call_2", "name": "exec", "arguments": `{"cmd":"pwd"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "a"},
		map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "b"},
	}
	raw, _ := json.Marshal(map[string]any{
		"model": "gemini-3-flash", "input": input,
		"tools": []any{map[string]any{"type": "function", "name": "exec", "description": "run", "parameters": map[string]any{"type": "object"}}},
	})
	converted, err := convertResponsesToGemini(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(converted.geminiBody, "contents.1.parts")
	if parts.Get("0").Raw != rawPart {
		t.Fatalf("real signature changed: %s", parts.Get("0").Raw)
	}
	if parts.Get("1.functionCall.args.cmd").String() != "pwd" || parts.Get("1.thoughtSignature").String() != geminiBypassThoughtSignature {
		t.Fatalf("later call = %s", parts.Get("1").Raw)
	}
}

func TestGeminiStringMessagesAreObjectParts(t *testing.T) {
	for _, role := range []string{"user", "assistant", "developer"} {
		input := []any{map[string]any{"role": role, "content": "hello\n\"world\""}}
		path := "contents.0.parts.0"
		if role == "developer" {
			input = append(input, map[string]any{"role": "user", "content": "hi"})
			path = "systemInstruction.parts.0"
		}
		raw, _ := json.Marshal(map[string]any{"model": "gemini-test", "input": input})
		converted, err := convertResponsesToGemini(raw)
		if err != nil {
			t.Fatal(err)
		}
		part := gjson.GetBytes(converted.geminiBody, path)
		if !part.IsObject() || part.Get("text").String() != "hello\n\"world\"" {
			t.Fatalf("invalid %s part: %s", role, part.Raw)
		}
	}
}

func TestProtocolStreamErrorsDoNotFinishSuccessfully(t *testing.T) {
	for _, protocol := range []string{"chat", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			a := newResponseAssembler(&adapterRequest{}, nil)
			var err error
			switch protocol {
			case "chat":
				err = processChatCompletionsStream(strings.NewReader("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"), a)
			case "gemini":
				err = processGeminiStream(strings.NewReader("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"), a)
			}
			if err == nil || !strings.Contains(err.Error(), "upstream failed") || a.finished {
				t.Fatalf("error=%v finished=%v", err, a.finished)
			}
		})
	}
}
