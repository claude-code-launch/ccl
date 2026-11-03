package codex

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeHistoryPreservesNativeStateAndReplaysCCLSummaries(t *testing.T) {
	for _, body := range []string{
		`{"model":"native","store":false,"input":[{"type":"reasoning","id":"rs_native","encrypted_content":"native-encrypted","summary":[]}]}`,
		`{"model":"native","store":true,"input":[{"type":"reasoning","id":"rs_stored","summary":[]}]}`,
		`{"model":"native","input":"hello"}`,
	} {
		if got := nativeHistory([]byte(body)); !bytes.Equal(got, []byte(body)) {
			t.Fatalf("native-only request changed: %s", got)
		}
	}
	body, _ := json.Marshal(map[string]any{"model": "native", "store": false, "include": []string{"reasoning.encrypted_content"}, "input": []any{
		map[string]any{"type": "reasoning", "id": "rs_legacy_ccl", "summary": []any{}},
		map[string]any{"type": "reasoning", "id": "rs_native", "encrypted_content": "native-encrypted", "summary": []any{}},
		map[string]any{"type": "compaction", "id": "cmp_ccl", "status": "completed", "encrypted_content": encodeCompactionSummary("keep-this-goal")},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}})
	var request map[string]any
	if err := json.Unmarshal(nativeHistory(body), &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	if len(items) != 3 || request["store"] != false || request["include"] == nil || items[0].(map[string]any)["encrypted_content"] != "native-encrypted" {
		t.Fatalf("native state lost: %+v", request)
	}
	summary := items[1].(map[string]any)
	if summary["type"] != "message" || summary["encrypted_content"] != nil || summary["id"] != nil || summary["status"] != nil || !strings.Contains(textValue(summary["content"]), "keep-this-goal") {
		t.Fatalf("CCL summary not replayed: %+v", summary)
	}
}

func TestCodexSwitchesBetweenConvertedAndNativeHistory(t *testing.T) {
	var nativeCalls, convertedCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-native-key" || r.Header.Get("X-Api-Key") != "" {
			t.Error("provider credentials crossed into native route")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw := textValue(body)
		if body["model"] != "native-model" || body["store"] != false || !strings.Contains(raw, "keep-this-goal") || !strings.Contains(raw, "tool-result") {
			t.Errorf("native replay lost conversation: %s", raw)
		}
		for _, item := range body["input"].([]any) {
			if item.(map[string]any)["type"] == "reasoning" {
				writeError(w, 404, "invalid_request_error", "unpersisted reasoning item not found", "")
				return
			}
		}
		writeJSON(w, 200, map[string]any{"output": []any{map[string]any{"type": "reasoning", "id": "rs_native", "encrypted_content": "native-encrypted", "summary": []any{}}, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "native-continued"}}}}})
	}))
	t.Cleanup(native.Close)
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if convertedCalls.Add(1) == 1 {
			writeMessage(w, []map[string]any{{"type": "thinking", "thinking": "fixture thinking"}, {"type": "tool_use", "id": "call_local", "name": findTool(t, body, "lookup"), "input": map[string]any{}}})
			return
		}
		raw := textValue(body["messages"])
		if !strings.Contains(raw, "native-continued") || !strings.Contains(raw, "tool-result") || strings.Contains(raw, "native-encrypted") {
			t.Errorf("converted replay lost context or leaked opaque state: %s", raw)
		}
		writeMessage(w, []map[string]any{{"type": "text", "text": "converted-continued"}})
	}, "anthropic", Options{Desktop: true, OpenAIURL: native.URL, ChatGPTURL: native.URL})
	tools := []any{map[string]any{"type": "function", "name": "lookup"}}
	first := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": "keep-this-goal", "tools": tools}))
	history := []any{map[string]any{"role": "user", "content": "keep-this-goal"}}
	history = append(history, first["output"].([]any)...)
	// Cover conversations created by the previous adapter as well.
	history = append(history, map[string]any{"type": "reasoning", "id": "rs_old_adapter", "summary": []any{}}, map[string]any{"type": "function_call_output", "call_id": "call_local", "output": "tool-result"}, map[string]any{"role": "user", "content": "continue"})
	second := responseObject(t, postResponses(t, g, map[string]any{"model": "native-model", "store": false, "input": history}))
	history = append(history, second["output"].([]any)...)
	history = append(history, map[string]any{"role": "user", "content": "continue again"})
	third := responseObject(t, postResponses(t, g, map[string]any{"model": "ccl/chosen/model-a", "input": history, "tools": tools}))
	if nativeCalls.Load() != 1 || convertedCalls.Load() != 2 || !strings.Contains(textValue(third), "converted-continued") {
		t.Fatal("model switching did not continue the conversation")
	}
}
