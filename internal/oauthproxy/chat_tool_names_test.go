package oauthproxy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func chatToolNameFixture(t *testing.T, names ...string) *chatCompletionsConvertedRequest {
	t.Helper()
	var tools []any
	for _, name := range names {
		tools = append(tools, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
	}
	raw, _ := json.Marshal(map[string]any{"model": "fixture", "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}})
	converted, err := convertAnthropicToChatCompletions(raw)
	if err != nil {
		t.Fatal(err)
	}
	return converted
}

func TestChatToolNameStreamPreservesFragmentsAndCompleteNames(t *testing.T) {
	for _, fragments := range [][]string{{"look", "up"}, {"lookup", "lookup"}, {"look", "lookup"}, {"lookup"}} {
		t.Run(strings.Join(fragments, "+"), func(t *testing.T) {
			converted := chatToolNameFixture(t, "lookup")
			var stream strings.Builder
			for i, name := range fragments {
				call := map[string]any{"index": 0, "function": map[string]any{"name": name}}
				if i == 0 {
					call["id"] = "call_1"
					call["function"].(map[string]any)["arguments"] = `{"key":"value"}`
				}
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}}})
				fmt.Fprintf(&stream, "data: %s\n\n", raw)
			}
			stream.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, nil)
			if err := processChatCompletionsStream(strings.NewReader(stream.String()), assembler); err != nil {
				t.Fatal(err)
			}
			blocks := assembler.contentBlocks()
			if len(blocks) != 1 || blocks[0].Name != "lookup" || blocks[0].ID != "call_1" || (*blocks[0].Input)["key"] != "value" {
				t.Fatalf("fragmented call changed: %+v", blocks)
			}
		})
	}
}

func TestChatMalformedToolNamesNeverBecomeCalls(t *testing.T) {
	const malformed = "exec_f4d... wait typo private-reasoning <｜end▁of▁thinking｜>\n<｜DSML｜calls>"
	for _, names := range [][]string{{"lookup"}, nil} {
		for _, stream := range []bool{false, true} {
			converted := chatToolNameFixture(t, names...)
			writer := httptest.NewRecorder()
			assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
			message := map[string]any{"tool_calls": []any{map[string]any{"id": "bad-call", "index": 0, "type": "function", "function": map[string]any{"name": malformed, "arguments": `{"key":"private-arguments"}`}}}}
			var err error
			if stream {
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": message, "finish_reason": "tool_calls"}}})
				err = processChatCompletionsStream(strings.NewReader("data: "+string(raw)+"\n\n"), assembler)
			} else {
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}}})
				err = processChatCompletionsNonStream(raw, assembler)
			}
			if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "DSML") {
				t.Fatalf("corrupt name was accepted or exposed: %v", err)
			}
			if assembler.hasToolUse || assembler.finished || strings.Contains(writer.Body.String(), `"type":"tool_use"`) {
				t.Fatal("corrupt name was emitted as a client-executable call")
			}
		}
	}
}

func TestChatAmbiguousNameFragmentsFailClosed(t *testing.T) {
	declared := map[string]struct{}{"a": {}, "aa": {}}
	if _, err := chatToolNameDelta("a", "a", declared); err == nil {
		t.Fatal("repeated name and a different fragmented name were guessed between")
	}
	converted := chatToolNameFixture(t, "lookup")
	writer := httptest.NewRecorder()
	assembler := newAnthropicResponseAssembler(&converted.anthropicAdapterRequest, writer)
	state := &chatCompletionsStreamState{assembler: assembler, tools: map[string]*chatToolCall{
		"0": {index: 0, id: "valid", name: "lookup"},
		"1": {index: 1, id: "incomplete", name: "look"},
	}}
	if err := state.emitTools(); err == nil || assembler.hasToolUse || strings.Contains(writer.Body.String(), `"type":"tool_use"`) {
		t.Fatal("valid parallel call escaped before the entire batch was validated")
	}
}
