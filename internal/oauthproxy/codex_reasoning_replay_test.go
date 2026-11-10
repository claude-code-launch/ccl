package oauthproxy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesNativeReasoningRoundTrip(t *testing.T) {
	// Two independent native items straddle a tool call. The second has no
	// visible summary; both must survive terminal duplication and client storage.
	output := []map[string]any{
		{"id": "rs_a", "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": "checking"}}, "content": nil, "encrypted_content": "opaque+/=\n", "future_field": "keep"},
		{"id": "fc_a", "type": "function_call", "call_id": "call_a", "name": "lookup", "arguments": "{}"},
		{"id": "rs_b", "type": "reasoning", "status": "completed", "summary": []any{}, "encrypted_content": "second"},
	}
	for _, terminalOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminalOnly=%t", terminalOnly), func(t *testing.T) {
			var stream strings.Builder
			if !terminalOnly {
				for index, item := range output {
					event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
					fmt.Fprintf(&stream, "data: %s\n\n", event)
				}
			}
			terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": output}})
			fmt.Fprintf(&stream, "data: %s\n\n", terminal)
			writer := httptest.NewRecorder()
			assembler := newResponseAssembler(&adapterRequest{stream: true}, writer)
			if err := processCodexResponsesStream(strings.NewReader(stream.String()), assembler); err != nil {
				t.Fatal(err)
			}
			blocks := assembler.contentBlocks()
			if len(blocks) != 3 {
				t.Fatalf("got %d blocks, want reasoning/tool/reasoning", len(blocks))
			}
			var signatures []string
			if err := readCodexSSE(strings.NewReader(writer.Body.String()), func(raw []byte) error {
				var event map[string]any
				if err := json.Unmarshal(raw, &event); err != nil {
					return err
				}
				if stringValue(event["type"]) == "response.output_item.done" {
					if item := mapValue(event["item"]); stringValue(item["type"]) == "reasoning" {
						signatures = append(signatures, stringValue(item["encrypted_content"]))
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(signatures) != 2 || signatures[0] != blocks[0].Signature || signatures[1] != blocks[2].Signature {
				t.Fatal("SSE signatures differ from persisted content")
			}
			reqJSON, _ := json.Marshal(map[string]any{
				"model": "gpt-5",
				"tools": []any{map[string]any{"type": "function", "name": "lookup"}},
				"input": assembler.outputItems(),
			})
			parsed, err := parseResponsesRequest(reqJSON)
			if err != nil || len(parsed.Turns) != 1 {
				t.Fatalf("parse replay turns: err=%v turns=%d", err, len(parsed.Turns))
			}
			items, err := codexTurnItems(parsed.Turns[0])
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(items)
			var replay []map[string]any
			if err := json.Unmarshal(wire, &replay); err != nil {
				t.Fatal(err)
			}
			if len(replay) != 3 || replay[1]["type"] != "function_call" {
				t.Fatalf("replay order = %s", wire)
			}
			for _, index := range []int{0, 2} {
				if !reflect.DeepEqual(replay[index], output[index]) {
					t.Fatalf("native reasoning changed: got %#v, want %#v", replay[index], output[index])
				}
			}
		})
	}
}

func TestResponsesReasoningRejectsCorruptEnvelope(t *testing.T) {
	for _, signature := range []string{codexReasoningSignaturePrefix + "broken", codexReasoningSignaturePrefix, "old+/="} {
		if _, err := codexReasoningFromSignature(signature); err == nil {
			t.Fatalf("corrupt or unprefixed envelope accepted: %q", signature)
		}
	}
}

func TestResponsesRuntimeIdentityStableAcrossTurns(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hi"}]}`)
	first, err := convertResponsesForUpstream(raw, "runtime-session")
	if err != nil {
		t.Fatal(err)
	}
	second, err := convertResponsesForUpstream(raw, "runtime-session")
	if err != nil {
		t.Fatal(err)
	}
	if first.sessionID != "runtime-session" || second.sessionID != first.sessionID {
		t.Fatalf("unstable identity: %q %q", first.sessionID, second.sessionID)
	}
	explicitRaw := []byte(`{"model":"gpt-5","prompt_cache_key":"123e4567-e89b-12d3-a456-426614174000","input":[{"role":"user","content":"hi"}]}`)
	explicit, err := convertResponsesForUpstream(explicitRaw, "runtime-session")
	if err != nil || !strings.Contains(string(explicit.body), `"prompt_cache_key":"123e4567-e89b-12d3-a456-426614174000"`) {
		t.Fatalf("explicit prompt_cache_key lost: err=%v body=%s", err, explicit.body)
	}
}
