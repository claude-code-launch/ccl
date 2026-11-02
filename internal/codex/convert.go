package codex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
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
}

type toolSpec struct{ Name, Namespace, Kind, WireName string }
type toolRegistry map[string]toolSpec

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

func convertTools(tools []map[string]any) ([]map[string]any, toolRegistry, error) {
	registry := make(toolRegistry)
	var result []map[string]any
	var add func(map[string]any, string) error
	add = func(tool map[string]any, namespace string) error {
		kind, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if kind == "namespace" {
			children, _ := tool["tools"].([]any)
			for _, child := range children {
				item, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid namespaced tool")
				}
				if err := add(item, name); err != nil {
					return err
				}
			}
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("tool type %q is not supported by this CCL provider; use local function tools", kind)
		}
		if name == "" {
			return fmt.Errorf("tool name is required")
		}
		wire := toolWireName(namespace, name)
		if _, exists := registry[wire]; exists {
			return fmt.Errorf("duplicate tool definition")
		}
		registry[wire] = toolSpec{Name: name, Namespace: namespace, Kind: kind, WireName: wire}
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
		result = append(result, map[string]any{"name": wire, "description": description, "input_schema": schema})
		if len(result) > 1024 {
			return fmt.Errorf("too many tool definitions")
		}
		return nil
	}
	for _, tool := range tools {
		if err := add(tool, ""); err != nil {
			return nil, nil, err
		}
	}
	return result, registry, nil
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
	tools, registry, err := convertTools(request.Tools)
	if err != nil {
		return nil, nil, err
	}
	system := request.Instructions
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
		case "function_call", "custom_tool_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			ns, _ := item["namespace"].(string)
			if id == "" || name == "" {
				return nil, nil, fmt.Errorf("tool call requires call_id and name")
			}
			input := make(map[string]any)
			if kind == "custom_tool_call" {
				input["input"] = textValue(item["input"])
			} else if args, _ := item["arguments"].(string); args != "" {
				if json.Unmarshal([]byte(args), &input) != nil || input == nil {
					return nil, nil, fmt.Errorf("function arguments must be a JSON object")
				}
			}
			appendMessage("assistant", []map[string]any{{"type": "tool_use", "id": id, "name": toolWireName(ns, name), "input": input}})
		case "function_call_output", "custom_tool_call_output":
			id, _ := item["call_id"].(string)
			if id == "" {
				return nil, nil, fmt.Errorf("tool output requires call_id")
			}
			appendMessage("user", []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": textValue(item["output"])}})
		case "reasoning":
			if value, _ := item["encrypted_content"].(string); value != "" {
				return nil, nil, fmt.Errorf("opaque reasoning from a different backend cannot be replayed to this provider")
			}
		case "compaction":
			return nil, nil, fmt.Errorf("opaque native compaction cannot be replayed to this provider")
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
	if effort, ok := request.Reasoning["effort"].(string); ok && effort != "" && effort != "none" {
		body["output_config"] = map[string]any{"effort": effort}
	}
	if format, ok := request.Text["format"].(map[string]any); ok && format["type"] == "json_schema" {
		body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": format["schema"]}}
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
	if compact {
		request.Instructions = "Summarize the conversation for another coding assistant to continue. Retain the user's goals, constraints, decisions, tool results, changed files, and outstanding tasks. Do not perform the tasks or call tools.\n" + request.Instructions
		request.Tools = nil
		request.ToolChoice = nil
		request.Stream = false
	}
	body, tools, err := toMessages(request, items, route)
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error(), "")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.runtime.ClaudeBaseURL()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		writeError(w, 502, "api_error", "invalid CCL runtime", "")
		return
	}
	upstream.Header.Set("X-Api-Key", g.runtime.APIKey())
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")
	response, err := g.client.Do(upstream)
	if err != nil {
		writeError(w, 502, "api_error", "CCL provider connection failed", "")
		return
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
		writeError(w, response.StatusCode, "api_error", failure.Error.Message, "")
		return
	}
	stream := newResponseStream(w, request.Model, request.Stream, tools)
	if request.Stream {
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
		stream.fail(err)
		return
	}
	output := stream.outputItems()
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
