package codex

import (
	"encoding/json"
	"strings"
)

// nativeHistory removes adapter-only state before a native Responses request.
// Plain reasoning summaries cannot reference OpenAI state when store is false.
// Native encrypted reasoning and all conversation/tool items remain intact.
func nativeHistory(body []byte) []byte {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(request["input"], &items) != nil {
		return body
	}
	var store bool
	stateless := json.Unmarshal(request["store"], &store) == nil && !store
	changed := false
	result := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		var kind, id, encrypted string
		_ = json.Unmarshal(item["type"], &kind)
		_ = json.Unmarshal(item["id"], &id)
		_ = json.Unmarshal(item["encrypted_content"], &encrypted)
		if kind == "reasoning" && (strings.HasPrefix(id, "rs_ccl_") || encrypted == "" && stateless) {
			changed = true
			continue
		}
		if kind == "compaction" {
			if summary, ok := decodeCompactionSummary(encrypted); ok {
				raw, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Conversation summary:\n" + summary}}})
				item = make(map[string]json.RawMessage)
				_ = json.Unmarshal(raw, &item)
				changed = true
			}
		}
		result = append(result, item)
	}
	if !changed {
		return body
	}
	request["input"], _ = json.Marshal(result)
	raw, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return raw
}
