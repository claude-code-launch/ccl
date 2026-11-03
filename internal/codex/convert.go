package codex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type responseRequest struct {
	Model              string           `json:"model"`
	Input              json.RawMessage  `json:"input"`
	Instructions       string           `json:"instructions"`
	Tools              []map[string]any `json:"tools"`
	ToolChoice         any              `json:"tool_choice"`
	Stream             bool             `json:"stream"`
	PreviousResponseID string           `json:"previous_response_id"`
	MaxOutputTokens    int              `json:"max_output_tokens"`
	Reasoning          map[string]any   `json:"reasoning"`
	Text               map[string]any   `json:"text"`
	ClientMetadata     map[string]any   `json:"client_metadata"`
}

type toolSpec struct{ Name, Namespace, Kind, WireName string }
type toolRegistry map[string]toolSpec

func hostedResponsesTool(kind string) bool {
	switch kind {
	case "web_search", "web_search_preview", "image_generation", "code_interpreter", "file_search",
		"computer", "computer_use_preview", "tool_search", "mcp", "local_shell":
		return true
	default:
		return strings.HasPrefix(kind, "web_search") || strings.HasPrefix(kind, "computer_use")
	}
}

func toolWireName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	var out strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out.WriteRune(c)
		} else {
			out.WriteByte('_')
		}
		if out.Len() >= 35 {
			break
		}
	}
	return out.String() + "_" + hex.EncodeToString(sum[:6])
}

func convertTools(tools []map[string]any, discovered ...[]map[string]any) ([]map[string]any, toolRegistry, error) {
	registry := make(toolRegistry)
	var result []map[string]any
	positions := make(map[string]int)
	explicit := make(map[string]bool)
	hasSearch := false
	for _, tool := range tools {
		if tool["type"] == "tool_search" && tool["execution"] == "client" {
			hasSearch = true
		}
	}
	var add func(map[string]any, string, bool) error
	add = func(tool map[string]any, namespace string, loaded bool) error {
		kind, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if kind == "namespace" {
			children, _ := tool["tools"].([]any)
			for _, child := range children {
				item, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid namespaced tool")
				}
				if err := add(item, name, loaded); err != nil {
					return err
				}
			}
			return nil
		}
		specKind := kind
		if kind == "tool_search" && tool["execution"] == "client" {
			kind, name = "function", "tool_search"
		}
		if kind != "function" && kind != "custom" {
			// A translated provider cannot execute OpenAI-hosted tools.
			// Client tool search is adapted above; retain the local function
			// tools and drop unsupported hosted declarations.
			if hostedResponsesTool(kind) {
				return nil
			}
			return fmt.Errorf("tool type %q is not supported by this CCL provider; use local function tools", kind)
		}
		if name == "" {
			return fmt.Errorf("tool name is required")
		}
		if deferred, _ := tool["defer_loading"].(bool); deferred && hasSearch && !loaded {
			return nil
		}
		wire := toolWireName(namespace, name)
		if _, exists := registry[wire]; exists {
			if !loaded {
				return fmt.Errorf("duplicate tool definition")
			}
			if explicit[wire] {
				return nil
			}
		}
		registry[wire] = toolSpec{Name: name, Namespace: namespace, Kind: specKind, WireName: wire}
		if !loaded {
			explicit[wire] = true
		}
		schema := tool["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		description, _ := tool["description"].(string)
		if kind == "custom" {
			schema = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The complete raw input for the tool."}}, "required": []string{"input"}, "additionalProperties": false}
			if format, ok := tool["format"].(map[string]any); ok {
				raw, _ := json.Marshal(format)
				description += "\nThe input must follow this format: " + string(raw)
			}
		}
		definition := map[string]any{"name": wire, "description": description, "input_schema": schema}
		if position, exists := positions[wire]; exists {
			result[position] = definition
		} else {
			positions[wire] = len(result)
			result = append(result, definition)
		}
		if len(result) > 1024 {
			return fmt.Errorf("too many tool definitions")
		}
		return nil
	}
	for _, tool := range tools {
		if err := add(tool, "", false); err != nil {
			return nil, nil, err
		}
	}
	for _, group := range discovered {
		for _, tool := range group {
			if err := add(tool, "", true); err != nil {
				return nil, nil, err
			}
		}
	}
	return result, registry, nil
}

const compactionSummaryPrefix = "ccl1:"

func encodeCompactionSummary(summary string) string {
	return compactionSummaryPrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

func decodeCompactionSummary(encoded string) (string, bool) {
	raw, ok := strings.CutPrefix(encoded, compactionSummaryPrefix)
	if !ok || raw == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
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
// so Messages backends do not require tool definitions or resume a tool loop.
func compactionItems(items []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(items)+1)
	for _, item := range items {
		kind, _ := item["type"].(string)
		switch kind {
		case "function_call", "custom_tool_call", "tool_search_call":
			input := item["arguments"]
			if kind == "custom_tool_call" {
				input = item["input"]
			}
			name := textValue(item["name"])
			if kind == "tool_search_call" {
				name = "tool_search"
			}
			result = append(result, map[string]any{"type": "message", "role": "assistant", "content": fmt.Sprintf("Previous tool call %s (%s):\n%s", name, textValue(item["call_id"]), textValue(input))})
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			output := item["output"]
			if kind == "tool_search_output" {
				output = item["tools"]
			}
			result = append(result, map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("Previous tool result (%s):\n%s", textValue(item["call_id"]), textValue(output))})
		default:
			result = append(result, item)
		}
	}
	return append(result, map[string]any{"type": "message", "role": "user", "content": "Summarize the conversation above for another assistant to continue. Preserve the goals, constraints, decisions, tool results, changed files, and outstanding tasks. Return only the summary; do not continue the task or call tools."})
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
	if json.Unmarshal(raw, &items) != nil {
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

func messageContent(value any) ([]map[string]any, error) {
	if text, ok := value.(string); ok {
		return []map[string]any{{"type": "text", "text": text}}, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Responses message content")
	}
	var result []map[string]any
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Responses content part")
		}
		switch p["type"] {
		case "input_text", "output_text", "text":
			result = append(result, map[string]any{"type": "text", "text": textValue(p["text"])})
		case "input_image":
			source, _ := p["image_url"].(string)
			if strings.HasPrefix(source, "data:") {
				meta, data, found := strings.Cut(strings.TrimPrefix(source, "data:"), ",")
				if !found || !strings.HasSuffix(meta, ";base64") {
					return nil, fmt.Errorf("image input must use a base64 data URL")
				}
				result = append(result, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": strings.TrimSuffix(meta, ";base64"), "data": data}})
			} else {
				return nil, fmt.Errorf("remote image inputs are not supported by this CCL adapter")
			}
		default:
			return nil, fmt.Errorf("unsupported Responses content part %q", p["type"])
		}
	}
	return result, nil
}

func toMessages(request responseRequest, items []map[string]any, route Route) ([]byte, toolRegistry, error) {
	discovered, err := discoveredResponseTools(items)
	if err != nil {
		return nil, nil, err
	}
	tools, registry, err := convertTools(request.Tools, discovered)
	if err != nil {
		return nil, nil, err
	}
	system := request.Instructions
	// Model/tool-mode switches can leave calls to tools that this request no
	// longer declares. Retain their contents as history, not callable examples.
	unavailableCalls := make(map[string]bool)
	for _, item := range items {
		switch item["type"] {
		case "function_call", "custom_tool_call", "tool_search_call":
			name, _ := item["name"].(string)
			namespace, _ := item["namespace"].(string)
			if item["type"] == "tool_search_call" {
				name, namespace = "tool_search", ""
			}
			id, _ := item["call_id"].(string)
			if _, ok := registry[toolWireName(namespace, name)]; !ok && id != "" {
				unavailableCalls[id] = true
			}
		}
	}
	if len(unavailableCalls) > 0 {
		system += "\nOnly tools in the current tools list may be called. Historical tool records are conversation context, not tool definitions."
	}
	messages := make([]map[string]any, 0, len(items))
	appendMessage := func(role string, content []map[string]any) {
		if len(content) == 0 {
			return
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			last := messages[len(messages)-1]
			last["content"] = append(last["content"].([]map[string]any), content...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	for _, item := range items {
		kind, _ := item["type"].(string)
		switch kind {
		case "", "message":
			role, _ := item["role"].(string)
			content, err := messageContent(item["content"])
			if err != nil {
				return nil, nil, err
			}
			if role == "developer" || role == "system" {
				for _, part := range content {
					if part["type"] != "text" {
						return nil, nil, fmt.Errorf("system content must be text")
					}
					system += "\n" + textValue(part["text"])
				}
				continue
			}
			if role != "user" && role != "assistant" {
				return nil, nil, fmt.Errorf("unsupported Responses message role")
			}
			appendMessage(role, content)
		case "function_call", "custom_tool_call", "tool_search_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			ns, _ := item["namespace"].(string)
			if kind == "tool_search_call" {
				name, ns = "tool_search", ""
			}
			if id == "" || name == "" {
				return nil, nil, fmt.Errorf("tool call requires call_id and name")
			}
			input := make(map[string]any)
			if kind == "custom_tool_call" {
				input["input"] = textValue(item["input"])
			} else if args, ok := item["arguments"].(map[string]any); ok && kind == "tool_search_call" {
				input = args
			} else if args, _ := item["arguments"].(string); args != "" {
				if json.Unmarshal([]byte(args), &input) != nil || input == nil {
					return nil, nil, fmt.Errorf("function arguments must be a JSON object")
				}
			}
			if unavailableCalls[id] {
				appendMessage("assistant", []map[string]any{{"type": "text", "text": fmt.Sprintf("Historical tool call %s (%s), unavailable in the current tool list:\n%s", name, id, textValue(input))}})
				continue
			}
			appendMessage("assistant", []map[string]any{{"type": "tool_use", "id": id, "name": toolWireName(ns, name), "input": input}})
		case "tool_search_output":
			id, _ := item["call_id"].(string)
			if id == "" {
				return nil, nil, fmt.Errorf("tool search output requires call_id")
			}
			found, err := discoveredResponseTools([]map[string]any{item})
			if err != nil {
				return nil, nil, err
			}
			definitions, _, err := convertTools(nil, found)
			if err != nil {
				return nil, nil, err
			}
			if definitions == nil {
				definitions = []map[string]any{}
			}
			if unavailableCalls[id] {
				appendMessage("user", []map[string]any{{"type": "text", "text": fmt.Sprintf("Historical tool result (%s):\n%s", id, textValue(definitions))}})
			} else {
				appendMessage("user", []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": textValue(definitions)}})
			}
		case "function_call_output", "custom_tool_call_output":
			id, _ := item["call_id"].(string)
			var content any = textValue(item["output"])
			if parts, ok := item["output"].([]any); ok {
				converted, err := messageContent(parts)
				if err != nil {
					return nil, nil, err
				}
				content = converted
			}
			if id == "" || unavailableCalls[id] {
				label := fmt.Sprintf("Historical tool result (%s):", id)
				if id == "" {
					// Desktop-delivered messages carry a named output without a
					// preceding model call. Do not invent a tool_use_id for them.
					name, _ := item["name"].(string)
					if name == "" {
						return nil, nil, fmt.Errorf("tool output requires call_id or a notification name")
					}
					namespace, _ := item["namespace"].(string)
					if namespace != "" {
						name = namespace + "." + name
					}
					label = "Tool notification from " + name + ":"
				}
				parts := []map[string]any{{"type": "text", "text": label}}
				if typed, ok := content.([]map[string]any); ok {
					parts = append(parts, typed...)
				} else {
					parts = append(parts, map[string]any{"type": "text", "text": textValue(content)})
				}
				appendMessage("user", parts)
				continue
			}
			appendMessage("user", []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": content}})
		case "reasoning":
			// Reasoning belongs to its backend. Keep the messages and tool
			// history when switching from native GPT to a converted model.
		case "compaction":
			encoded, _ := item["encrypted_content"].(string)
			summary, ok := decodeCompactionSummary(encoded)
			if !ok {
				return nil, nil, fmt.Errorf("opaque native compaction cannot be replayed to this provider")
			}
			appendMessage("user", []map[string]any{{"type": "text", "text": "Conversation summary:\n" + summary}})
		default:
			return nil, nil, fmt.Errorf("unsupported Responses input item %q", kind)
		}
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("Responses input has no conversation messages")
	}
	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := map[string]any{"model": route.Model, "max_tokens": maxTokens, "stream": true, "messages": messages}
	if system != "" {
		body["system"] = system
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	switch choice := request.ToolChoice.(type) {
	case string:
		switch choice {
		case "auto":
			body["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			body["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			body["tool_choice"] = map[string]any{"type": "none"}
		default:
			return nil, nil, fmt.Errorf("unsupported tool_choice")
		}
	case map[string]any:
		name, _ := choice["name"].(string)
		if choice["type"] == "tool_search" {
			name = "tool_search"
		}
		ns, _ := choice["namespace"].(string)
		wire := toolWireName(ns, name)
		if _, ok := registry[wire]; !ok {
			return nil, nil, fmt.Errorf("tool_choice is not in tools")
		}
		body["tool_choice"] = map[string]any{"type": "tool", "name": wire}
	case nil:
	default:
		return nil, nil, fmt.Errorf("unsupported tool_choice")
	}
	outputConfig := make(map[string]any)
	if effort, ok := request.Reasoning["effort"].(string); ok && effort != "" && effort != "none" {
		if route.ThinkingBudget {
			budget := map[string]int{"minimal": 1024, "low": 1024, "medium": 2048, "high": 4096, "xhigh": 8192, "max": 16384, "ultra": 32768}[effort]
			if budget == 0 {
				return nil, nil, fmt.Errorf("unsupported reasoning effort")
			}
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			if maxTokens <= budget {
				body["max_tokens"] = budget + 4096
			}
		} else {
			body["thinking"] = map[string]any{"type": "adaptive"}
			outputConfig["effort"] = effort
		}
	}
	if format, ok := request.Text["format"].(map[string]any); ok && format["type"] == "json_schema" {
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": format["schema"]}
	}
	if len(outputConfig) > 0 {
		body["output_config"] = outputConfig
	}
	raw, err := json.Marshal(body)
	return raw, registry, err
}

func (g *Gateway) serveBridge(w http.ResponseWriter, r *http.Request, request responseRequest, route Route, compact bool) {
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
		request.Tools = nil
		request.ToolChoice = nil
		items = compactionItems(items)
	}
	if compact {
		request.Stream = false
	}
	body, tools, err := toMessages(request, items, route)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error(), "")
		return
	}
	stream := newResponseStream(w, request.Model, request.Stream, tools)
	if pendingNotice && !compactionRequest {
		stream.notice = "CCL context migration: resumed from a bounded local transcript excerpt. The encrypted summary was not decrypted; older details may be missing.\n\n"
	}
	stream.suppress = triggered && !compact
	stream = g.readBridgeResponse(w, r, body, stream)
	if stream == nil {
		return
	}
	if pendingNotice && !compactionRequest {
		g.migrationNotices.remove(noticeID, "migration-notice", owner)
	}
	output := stream.outputItems()
	if triggered && !compact {
		summary := stream.summaryText()
		if summary == "" {
			writeError(w, 502, "api_error", "provider returned no compaction summary", "")
			return
		}
		if err := stream.writeCompaction(summary); err != nil {
			return
		}
		g.history.put(stream.id, route.Alias, owner, []map[string]any{{"type": "message", "role": "user", "content": "Conversation summary:\n" + summary}})
		return
	}
	if compact {
		var summary strings.Builder
		for _, item := range output {
			if content, ok := item["content"].([]any); ok {
				for _, part := range content {
					if p, ok := part.(map[string]any); ok {
						summary.WriteString(textValue(p["text"]))
					}
				}
			}
		}
		if summary.Len() == 0 {
			writeError(w, 502, "api_error", "provider returned no compaction summary", "")
			return
		}
		writeJSON(w, 200, map[string]any{"id": randomID("cmp_"), "object": "response.compaction", "created_at": time.Now().Unix(), "output": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Conversation summary:\n" + summary.String()}}}}, "usage": stream.usage})
		return
	}
	g.history.put(stream.id, route.Alias, owner, append(items, output...))
	stream.finish()
}

func (g *Gateway) readBridgeResponse(w http.ResponseWriter, r *http.Request, body []byte, stream *responseStream) *responseStream {
	fail := func(status int, message string) {
		if stream.stream && stream.started && !stream.suppress {
			stream.fail(fmt.Errorf("%s", message))
		} else {
			writeError(w, status, "api_error", message, "")
		}
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.runtime.ClaudeBaseURL()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		fail(502, "invalid CCL runtime")
		return nil
	}
	upstream.Header.Set("X-Api-Key", g.runtime.APIKey())
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")
	response, err := g.client.Do(upstream)
	if err != nil {
		fail(502, "CCL provider connection failed")
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if hint := response.Header.Get("Retry-After"); hint != "" {
			w.Header().Set("Retry-After", hint)
		}
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		var failure struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		if failure.Error.Message == "" {
			failure.Error.Message = http.StatusText(response.StatusCode)
		}
		if failure.Error.Type == "" {
			failure.Error.Type = "api_error"
		}
		writeError(w, response.StatusCode, failure.Error.Type, failure.Error.Message, "")
		return nil
	}
	if stream.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	}
	reader := bufio.NewReader(response.Body)
	for {
		first, peekErr := reader.Peek(1)
		if peekErr != nil || !strings.ContainsRune(" \r\n\t", rune(first[0])) {
			break
		}
		_, _ = reader.ReadByte()
	}
	first, _ := reader.Peek(1)
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") && (len(first) == 0 || first[0] != '{') {
		err = stream.read(reader)
	} else {
		err = stream.readJSON(reader)
	}
	if err != nil {
		fail(502, err.Error())
		return nil
	}
	return stream
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
	if json.Unmarshal(e.Items, &items) != nil {
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
	for len(h.entries) >= 64 || h.bytes+len(raw) > 64<<20 {
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
