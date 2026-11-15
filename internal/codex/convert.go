package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
)

type responseRequest struct {
	Model              string           `json:"model"`
	Input              json.RawMessage  `json:"input"`
	Instructions       string           `json:"instructions"`
	Tools              []map[string]any `json:"tools"`
	Stream             bool             `json:"stream"`
	PreviousResponseID string           `json:"previous_response_id"`
	MaxOutputTokens    int              `json:"max_output_tokens"`
	Reasoning          map[string]any   `json:"reasoning"`
	ClientMetadata     map[string]any   `json:"client_metadata"`
}

// hoistCompactionTrigger drops Codex remote-compaction markers. The marker is
// not model input; the caller summarizes the remaining items and returns one
// compaction item. A marker that is not last is still removed.
func hoistCompactionTrigger(items []map[string]any) ([]map[string]any, bool) {
	triggered := false
	rest := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if kind, _ := item["type"].(string); kind == "compaction_trigger" {
			triggered = true
			continue
		}
		rest = append(rest, item)
	}
	return rest, triggered
}

// Summarization has no live tools. Preserve historical calls/results as text
// so the summarization request cannot resume a tool loop, and omit inline
// screenshot/image payloads so compaction stays within model context limits.
func compactionItems(items []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(items)+1)
	for _, item := range items {
		kind, _ := item["type"].(string)
		switch kind {
		case "function_call", "custom_tool_call", "tool_search_call", "computer_call", "shell_call", "local_shell_call", "apply_patch_call":
			name, input := compactionToolCallSummary(kind, item)
			result = append(result, map[string]any{"type": "message", "role": "assistant", "content": fmt.Sprintf("Previous tool call %s (%s):\n%s", name, textValue(item["call_id"]), compactionTextValue(input))})
		case "function_call_output", "custom_tool_call_output", "tool_search_output", "computer_call_output", "shell_call_output", "local_shell_call_output", "apply_patch_call_output":
			output := item["output"]
			if kind == "tool_search_output" {
				output = item["tools"]
			}
			result = append(result, map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("Previous tool result (%s):\n%s", textValue(item["call_id"]), compactionTextValue(output))})
		case "message":
			result = append(result, sanitizeCompactionMessage(item))
		default:
			result = append(result, item)
		}
	}
	return append(result, map[string]any{"type": "message", "role": "user", "content": "Summarize the conversation above for another assistant to continue. Preserve the goals, constraints, decisions, tool results, changed files, and outstanding tasks. Return only the summary; do not continue the task or call tools."})
}

func compactionToolCallSummary(kind string, item map[string]any) (string, any) {
	switch kind {
	case "custom_tool_call":
		return textValue(item["name"]), item["input"]
	case "tool_search_call":
		if args, ok := item["arguments"]; ok && args != nil {
			return "tool_search", args
		}
		return "tool_search", item["execution"]
	case "computer_call":
		if actions, ok := item["actions"]; ok && actions != nil {
			return "computer", actions
		}
		return "computer", item["action"]
	case "shell_call", "local_shell_call":
		return "shell", item["action"]
	case "apply_patch_call":
		if op, ok := item["operation"]; ok && op != nil {
			return "apply_patch", op
		}
		return "apply_patch", item["patch"]
	default:
		return textValue(item["name"]), item["arguments"]
	}
}

func sanitizeCompactionMessage(item map[string]any) map[string]any {
	switch content := item["content"].(type) {
	case string:
		if !isInlineImageData(content) {
			return item
		}
		cloned := make(map[string]any, len(item))
		for k, v := range item {
			cloned[k] = v
		}
		cloned["content"] = "[image omitted]"
		return cloned
	case []any:
		changed := false
		partType := "input_text"
		if role, _ := item["role"].(string); role == "assistant" {
			partType = "output_text"
		}
		sanitized := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				sanitized = append(sanitized, raw)
				continue
			}
			if isImageContentItem(part) {
				changed = true
				sanitized = append(sanitized, map[string]any{"type": partType, "text": "[image omitted]"})
				continue
			}
			if text, ok := part["text"].(string); ok && isInlineImageData(text) {
				changed = true
				clonedPart := make(map[string]any, len(part))
				for k, v := range part {
					clonedPart[k] = v
				}
				clonedPart["text"] = "[image omitted]"
				sanitized = append(sanitized, clonedPart)
				continue
			}
			sanitized = append(sanitized, raw)
		}
		if !changed {
			return item
		}
		cloned := make(map[string]any, len(item))
		for k, v := range item {
			cloned[k] = v
		}
		cloned["content"] = sanitized
		return cloned
	default:
		return item
	}
}

func compactionTextValue(value any) string {
	if s, ok := value.(string); ok {
		if isInlineImageData(s) {
			return "[image omitted]"
		}
		return s
	}
	sanitized, changed := sanitizeCompactionValue(value)
	if !changed {
		return textValue(value)
	}
	if s, ok := sanitized.(string); ok {
		return s
	}
	raw, _ := json.Marshal(sanitized)
	return string(raw)
}

func sanitizeCompactionValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		if isInlineImageData(v) {
			return "[image omitted]", true
		}
		return v, false
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, elem := range v {
			next, elemChanged := sanitizeCompactionValue(elem)
			changed = changed || elemChanged
			out = append(out, next)
		}
		return out, changed
	case map[string]any:
		if isImageContentItem(v) {
			return "[image omitted]", true
		}
		changed := false
		out := make(map[string]any, len(v))
		for k, elem := range v {
			if (k == "image_url" || k == "file_data") && elem != nil {
				out[k] = "[image omitted]"
				changed = true
				continue
			}
			next, elemChanged := sanitizeCompactionValue(elem)
			changed = changed || elemChanged
			out[k] = next
		}
		return out, changed
	default:
		return value, false
	}
}

func isImageContentItem(item map[string]any) bool {
	kind, _ := item["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "input_image", "computer_screenshot", "image", "image_url":
		return true
	}
	if urlValue, ok := item["image_url"].(string); ok && urlValue != "" {
		return true
	}
	return false
}

func isInlineImageData(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(trimmed, "data:image/")
}

func inputItems(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"type": "message", "role": "user", "content": text}}, nil
	}
	var items []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&items) != nil {
		return nil, fmt.Errorf("Responses input must be text or an array of items")
	}
	return items, nil
}

func textValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func (g *Gateway) serveBridge(w http.ResponseWriter, r *http.Request, body []byte, request responseRequest, route Route, compact bool) {
	items, err := inputItems(request.Input)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error(), "")
		return
	}
	owner := historyOwner(r)
	if request.PreviousResponseID != "" {
		previous, ok := g.history.get(request.PreviousResponseID, route.Alias, owner)
		if !ok {
			writeError(w, 400, "invalid_request_error", "previous_response_id is unknown, expired, or belongs to another route", "previous_response_not_found")
			return
		}
		items = append(previous, items...)
	}
	var migrated bool
	items, migrated, err = g.migrateNativeContext(r, route, items, request.ClientMetadata)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error(), "context_migration_unavailable")
		return
	}
	compactionRequest := isClientCompaction(r, request, items, r.URL.Path)
	noticeID := migrationSessionID(r, request.ClientMetadata)
	if migrated {
		g.migrationNotices.put(noticeID, "migration-notice", owner, []map[string]any{{"pending": true}})
	}
	_, pendingNotice := g.migrationNotices.get(noticeID, "migration-notice", owner)
	items, triggered := hoistCompactionTrigger(items)
	if compact || triggered {
		// Codex remote compaction v2 appends compaction_trigger to an ordinary
		// Responses request and expects exactly one compaction output item.
		// /responses/compact uses the same summary.
		request.Instructions = "Summarize the conversation for another coding assistant to continue. Retain the user's goals, constraints, decisions, tool results, changed files, and outstanding tasks. Do not perform the tasks or call tools.\n" + request.Instructions
		items = compactionItems(items)
	}
	if compact {
		request.Stream = false
	}
	// The runtime handles tool translation and Responses assembly. Send full
	// history because the gateway owns its local previous_response_id cache.
	upstreamBody, err := bridgeRequest(body, request, items, route, compact || triggered)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	response, err := g.callRuntime(r, upstreamBody, "/responses")
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "CCL provider connection failed", "")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		relayResponse(w, response)
		return
	}

	var notice string
	if pendingNotice && !compactionRequest {
		notice = "CCL context migration: resumed from a bounded local transcript excerpt. The encrypted summary was not decrypted; older details may be missing.\n\n"
	}
	record := func(result map[string]any) {
		if result["status"] != "completed" && result["status"] != "incomplete" {
			return
		}
		if notice != "" {
			g.migrationNotices.remove(noticeID, "migration-notice", owner)
		}
		id, _ := result["id"].(string)
		output, _ := result["output"].([]any)
		for _, value := range output {
			if item, ok := value.(map[string]any); ok {
				items = append(items, item)
			}
		}
		g.history.put(id, route.Alias, owner, items)
	}
	if request.Stream && !triggered {
		relayBridgeStream(w, response.Body, request.Model, notice, record)
		return
	}
	result, err := readBridgeJSON(response.Body, request.Model)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", err.Error(), "")
		return
	}
	if compact || triggered {
		summary := responseSummary(result)
		if summary == "" || result["status"] != "completed" {
			writeError(w, http.StatusBadGateway, "api_error", "provider did not complete the compaction summary", "")
			return
		}
		summaryItem := map[string]any{"type": "message", "role": "user", "content": "Conversation summary:\n" + summary}
		if triggered && !compact {
			id := randomID("resp_")
			g.history.put(id, route.Alias, owner, []map[string]any{summaryItem})
			writeBridgeCompaction(w, request, id, summary, result["usage"])
		} else {
			summaryItem["content"] = []any{map[string]any{"type": "input_text", "text": summaryItem["content"]}}
			writeJSON(w, http.StatusOK, map[string]any{"id": randomID("cmp_"), "object": "response.compaction",
				"created_at": time.Now().Unix(), "output": []any{summaryItem}, "usage": result["usage"]})
		}
		return
	}
	if notice != "" {
		prependNotice(result, migrationNoticeItem(notice))
	}
	record(result)
	writeJSON(w, http.StatusOK, result)
}

func bridgeRequest(raw []byte, request responseRequest, items []map[string]any, route Route, summarizing bool) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"], _ = json.Marshal(route.Model)
	body["input"], _ = json.Marshal(items)
	body["instructions"], _ = json.Marshal(request.Instructions)
	body["stream"], _ = json.Marshal(request.Stream && !summarizing)
	body["store"] = json.RawMessage("false")
	delete(body, "previous_response_id")
	if summarizing {
		delete(body, "tools")
		delete(body, "tool_choice")
		delete(body, "parallel_tool_calls")
		delete(body, "text")
	}
	if request.MaxOutputTokens <= 0 {
		budget := route.MaxOutputTokens
		if budget <= 0 {
			budget = 32768
		}
		body["max_output_tokens"], _ = json.Marshal(budget)
	}
	return json.Marshal(body)
}

func readBridgeJSON(body io.Reader, model string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(body, bridgeResponseLimit+1))
	if err != nil || len(raw) > bridgeResponseLimit {
		return nil, fmt.Errorf("provider response exceeds limit or was interrupted")
	}
	var result map[string]any
	if err := decodeBridgeJSON(raw, &result); err != nil || result["object"] != "response" {
		return nil, fmt.Errorf("invalid Responses payload from CCL runtime")
	}
	result["model"] = model
	return result, nil
}

func decodeBridgeJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func responseSummary(result map[string]any) string {
	var summary strings.Builder
	output, _ := result["output"].([]any)
	for _, entry := range output {
		item, _ := entry.(map[string]any)
		content, _ := item["content"].([]any)
		for _, value := range content {
			part, _ := value.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				summary.WriteString(text)
			}
		}
	}
	return strings.TrimSpace(summary.String())
}

func encodeCompactionSummary(summary string) string {
	return oauthproxy.EncodeCompactionSummary(summary)
}

func decodeCompactionSummary(encoded string) (string, bool) {
	return oauthproxy.DecodeCompactionSummary(encoded)
}

type historyEntry struct {
	Route, Owner string
	Items        []byte
	At           time.Time
}
type responseHistory struct {
	sync.Mutex
	entries map[string]historyEntry
	bytes   int
}

func (h *responseHistory) get(id, route, owner string) ([]map[string]any, bool) {
	h.Lock()
	defer h.Unlock()
	e, ok := h.entries[id]
	if !ok || e.Route != route || e.Owner != owner || time.Since(e.At) > 30*time.Minute {
		return nil, false
	}
	var items []map[string]any
	if decodeBridgeJSON(e.Items, &items) != nil {
		return nil, false
	}
	return items, true
}
func (h *responseHistory) put(id, route, owner string, items []map[string]any) {
	raw, err := json.Marshal(items)
	if err != nil || len(raw) > maxBodyBytes {
		return
	}
	h.Lock()
	defer h.Unlock()
	if h.entries == nil {
		h.entries = make(map[string]historyEntry)
	}
	for len(h.entries) >= 64 || h.bytes+len(raw) > 128<<20 {
		var oldest string
		var at time.Time
		for k, e := range h.entries {
			if oldest == "" || e.At.Before(at) {
				oldest = k
				at = e.At
			}
		}
		if oldest == "" {
			break
		}
		h.bytes -= len(h.entries[oldest].Items)
		delete(h.entries, oldest)
	}
	h.entries[id] = historyEntry{Route: route, Owner: owner, Items: raw, At: time.Now()}
	h.bytes += len(raw)
}

func (h *responseHistory) remove(id, route, owner string) {
	h.Lock()
	defer h.Unlock()
	if e, ok := h.entries[id]; ok && e.Route == route && e.Owner == owner {
		h.bytes -= len(e.Items)
		delete(h.entries, id)
	}
}
