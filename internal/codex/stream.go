package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type responseBlock struct {
	id, kind string
	spec     toolSpec
	text     strings.Builder
	args     strings.Builder
	closed   bool
}
type responseStream struct {
	w                      http.ResponseWriter
	id, model              string
	created                int64
	stream, started, ended bool
	suppress               bool
	seq                    int
	tools                  toolRegistry
	blocks                 []*responseBlock
	indexes                map[int]int
	usage                  map[string]any
	stopReason             string
	retained               int
	notice                 string
}

func responseUsage() map[string]any {
	return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

func newResponseStream(w http.ResponseWriter, model string, stream bool, tools toolRegistry) *responseStream {
	return &responseStream{w: w, id: randomID("resp_"), model: model, created: time.Now().Unix(), stream: stream,
		tools: tools, indexes: make(map[int]int), usage: responseUsage()}
}

func (s *responseStream) response(status string) map[string]any {
	output := s.outputItems()
	if output == nil {
		output = []map[string]any{}
	}
	result := map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": status, "model": s.model, "output": output, "usage": s.usage, "error": nil, "incomplete_details": nil, "parallel_tool_calls": false}
	if status == "incomplete" {
		result["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return result
}
func (s *responseStream) emit(event string, data map[string]any) error {
	if !s.stream || s.suppress {
		return nil
	}
	data["type"] = event
	data["sequence_number"] = s.seq
	s.seq++
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}
func (s *responseStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{"response": s.response("in_progress")}); err != nil {
		return err
	}
	if err := s.emit("response.in_progress", map[string]any{"response": s.response("in_progress")}); err != nil {
		return err
	}
	if s.notice != "" && !s.suppress {
		text := s.notice
		s.notice = ""
		if err := s.addBlock(4096, map[string]any{"type": "text", "text": text}); err != nil {
			return err
		}
		return s.closeBlock(4096)
	}
	return nil
}

func (s *responseStream) item(index int, complete bool) map[string]any {
	b := s.blocks[index]
	status := "in_progress"
	if complete {
		status = "completed"
	}
	switch b.kind {
	case "tool_use":
		if b.spec.Kind == "tool_search" {
			arguments := map[string]any{}
			_ = json.Unmarshal([]byte(b.args.String()), &arguments)
			return map[string]any{"id": "tsc_" + strings.TrimPrefix(b.id, "fc_"), "type": "tool_search_call", "call_id": strings.TrimPrefix(b.id, "fc_"), "execution": "client", "arguments": arguments, "status": status}
		}
		result := map[string]any{"id": b.id, "type": "function_call", "call_id": strings.TrimPrefix(b.id, "fc_"), "name": b.spec.Name, "arguments": b.args.String(), "status": status}
		if b.spec.Namespace != "" {
			result["namespace"] = b.spec.Namespace
		}
		if b.spec.Kind == "custom" {
			result["type"] = "custom_tool_call"
			delete(result, "arguments")
			var input map[string]any
			_ = json.Unmarshal([]byte(b.args.String()), &input)
			result["input"], _ = input["input"].(string)
		}
		return result
	case "thinking":
		return map[string]any{"id": b.id, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": b.text.String()}}, "status": status}
	default:
		return map[string]any{"id": b.id, "type": "message", "role": "assistant", "status": status, "content": []any{map[string]any{"type": "output_text", "text": b.text.String(), "annotations": []any{}}}}
	}
}
func (s *responseStream) outputItems() []map[string]any {
	var output []map[string]any
	for i, b := range s.blocks {
		output = append(output, s.item(i, b.closed))
	}
	return output
}
func (s *responseStream) addBlock(source int, block map[string]any) error {
	if source < 0 || source > 4096 || len(s.blocks) >= 4096 {
		return fmt.Errorf("provider returned too many content blocks")
	}
	if _, exists := s.indexes[source]; exists {
		return fmt.Errorf("provider repeated a content block")
	}
	if err := s.start(); err != nil {
		return err
	}
	kind, _ := block["type"].(string)
	if kind == "redacted_thinking" {
		return nil
	}
	if kind != "text" && kind != "thinking" && kind != "tool_use" {
		return fmt.Errorf("unsupported provider content block")
	}
	b := &responseBlock{kind: kind, id: randomID("msg_")}
	if kind == "tool_use" {
		name, _ := block["name"].(string)
		spec, ok := s.tools[name]
		if !ok {
			// Include names only: callers need to distinguish a stale history
			// name from a malformed provider name, without logging arguments.
			declared := make([]string, 0, len(s.tools))
			for wire := range s.tools {
				declared = append(declared, wire)
			}
			sort.Strings(declared)
			if len(declared) > 16 {
				declared = append(declared[:16], "...")
			}
			if !simpleToolName(name) {
				// A damaged name can contain model reasoning or other payload.
				// Do not expose it as diagnostics or split a UTF-8 sequence.
				name = "<malformed tool name omitted>"
			}
			return fmt.Errorf("provider called a tool outside the declared tool list: %q (declared: %s)", name, strings.Join(declared, ", "))
		}
		b.spec = spec
		id, _ := block["id"].(string)
		if id == "" {
			return fmt.Errorf("provider tool call has no identifier")
		}
		b.id = "fc_" + id
	}
	if kind == "thinking" {
		b.id = randomID("rs_ccl_")
	}
	i := len(s.blocks)
	s.indexes[source] = i
	s.blocks = append(s.blocks, b)
	if err := s.emit("response.output_item.added", map[string]any{"output_index": i, "item": s.item(i, false)}); err != nil {
		return err
	}
	if kind == "text" {
		if err := s.emit("response.content_part.added", map[string]any{"item_id": b.id, "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	} else if kind == "thinking" {
		if err := s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": b.id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}); err != nil {
			return err
		}
	}
	switch kind {
	case "text":
		if value, _ := block["text"].(string); value != "" {
			return s.delta(source, map[string]any{"type": "text_delta", "text": value})
		}
	case "thinking":
		if value, _ := block["thinking"].(string); value != "" {
			return s.delta(source, map[string]any{"type": "thinking_delta", "thinking": value})
		}
	case "tool_use":
		if input, ok := block["input"].(map[string]any); ok && len(input) > 0 {
			raw, _ := json.Marshal(input)
			return s.delta(source, map[string]any{"type": "input_json_delta", "partial_json": string(raw)})
		}
	}
	return nil
}
func (s *responseStream) delta(source int, delta map[string]any) error {
	i, ok := s.indexes[source]
	if !ok {
		return nil
	}
	b := s.blocks[i]
	if b.closed {
		return fmt.Errorf("provider wrote to a closed content block")
	}
	fields := map[string]any{"item_id": b.id, "output_index": i}
	var value, event string
	switch delta["type"] {
	case "text_delta":
		value, _ = delta["text"].(string)
		event = "response.output_text.delta"
		fields["content_index"] = 0
	case "thinking_delta":
		value, _ = delta["thinking"].(string)
		event = "response.reasoning_summary_text.delta"
		fields["summary_index"] = 0
	case "input_json_delta":
		value, _ = delta["partial_json"].(string)
		event = "response.function_call_arguments.delta"
	case "signature_delta":
		return nil
	default:
		return nil
	}
	s.retained += len(value)
	if s.retained > maxBodyBytes {
		return fmt.Errorf("provider response exceeds the CCL retention limit")
	}
	if b.kind == "tool_use" {
		b.args.WriteString(value)
		if b.spec.Kind == "custom" || b.spec.Kind == "tool_search" {
			return nil
		}
	} else {
		b.text.WriteString(value)
	}
	fields["delta"] = value
	return s.emit(event, fields)
}
func (s *responseStream) closeBlock(source int) error {
	i, ok := s.indexes[source]
	if !ok {
		return nil
	}
	b := s.blocks[i]
	if b.closed {
		return nil
	}
	fields := map[string]any{"item_id": b.id, "output_index": i}
	switch b.kind {
	case "text":
		fields["content_index"] = 0
		fields["text"] = b.text.String()
		if err := s.emit("response.output_text.done", fields); err != nil {
			return err
		}
		if err := s.emit("response.content_part.done", map[string]any{"item_id": b.id, "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.text.String(), "annotations": []any{}}}); err != nil {
			return err
		}
	case "thinking":
		fields["summary_index"] = 0
		fields["text"] = b.text.String()
		if err := s.emit("response.reasoning_summary_text.done", fields); err != nil {
			return err
		}
		if err := s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": b.id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.text.String()}}); err != nil {
			return err
		}
	case "tool_use":
		if b.args.Len() == 0 {
			b.args.WriteString("{}")
		}
		var input map[string]any
		if json.Unmarshal([]byte(b.args.String()), &input) != nil || input == nil {
			return fmt.Errorf("provider returned invalid tool arguments")
		}
		if b.spec.Kind == "tool_search" {
			// Search arguments are carried in the native output item. Codex
			// executes the search and sends tool_search_output next turn.
		} else if b.spec.Kind == "custom" {
			value, ok := input["input"].(string)
			if !ok {
				return fmt.Errorf("provider returned invalid custom tool input")
			}
			fields["delta"] = value
			if err := s.emit("response.custom_tool_call_input.delta", fields); err != nil {
				return err
			}
			delete(fields, "delta")
			fields["input"] = value
			if err := s.emit("response.custom_tool_call_input.done", fields); err != nil {
				return err
			}
		} else {
			fields["arguments"] = b.args.String()
			if err := s.emit("response.function_call_arguments.done", fields); err != nil {
				return err
			}
		}
	}
	b.closed = true
	return s.emit("response.output_item.done", map[string]any{"output_index": i, "item": s.item(i, true)})
}

func number(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
func (s *responseStream) updateUsage(usage map[string]any) {
	if value, ok := usage["input_tokens"]; ok {
		s.usage["input_tokens"] = number(value) + number(usage["cache_read_input_tokens"]) + number(usage["cache_creation_input_tokens"])
	}
	if value, ok := usage["output_tokens"]; ok {
		s.usage["output_tokens"] = number(value)
	}
	if value, ok := usage["cache_read_input_tokens"]; ok {
		s.usage["input_tokens_details"] = map[string]any{"cached_tokens": number(value)}
	}
	s.usage["total_tokens"] = number(s.usage["input_tokens"]) + number(s.usage["output_tokens"])
}
func (s *responseStream) event(raw []byte) error {
	var event map[string]any
	if json.Unmarshal(raw, &event) != nil {
		return fmt.Errorf("invalid provider stream event")
	}
	switch event["type"] {
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			if usage, ok := message["usage"].(map[string]any); ok {
				s.updateUsage(usage)
			}
		}
		return s.start()
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		return s.addBlock(number(event["index"]), block)
	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		return s.delta(number(event["index"]), delta)
	case "content_block_stop":
		return s.closeBlock(number(event["index"]))
	case "message_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			s.stopReason, _ = delta["stop_reason"].(string)
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			s.updateUsage(usage)
		}
	case "message_stop":
		s.ended = true
	case "error":
		message := "provider reported a stream error"
		if nested, ok := event["error"].(map[string]any); ok {
			if text, _ := nested["message"].(string); strings.TrimSpace(text) != "" {
				message = strings.TrimSpace(text)
			}
		}
		if len(message) > 500 {
			message = message[:500]
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}
func (s *responseStream) read(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data strings.Builder
	consume := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		return s.event([]byte(raw))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return err
			}
			if s.ended {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
			if data.Len() > 4<<20 {
				return fmt.Errorf("provider stream event exceeds limit")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("provider stream was interrupted")
	}
	if err := consume(); err != nil {
		return err
	}
	if !s.ended {
		return fmt.Errorf("provider stream ended without message_stop")
	}
	for source := range s.indexes {
		if err := s.closeBlock(source); err != nil {
			return err
		}
	}
	return nil
}
func (s *responseStream) readJSON(reader io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil || len(raw) > maxBodyBytes {
		return fmt.Errorf("provider response exceeds limit")
	}
	var message struct {
		Content    []map[string]any `json:"content"`
		Usage      map[string]any   `json:"usage"`
		StopReason string           `json:"stop_reason"`
		Type       string           `json:"type"`
	}
	if json.Unmarshal(raw, &message) != nil || message.Type != "message" {
		return fmt.Errorf("invalid provider message")
	}
	s.updateUsage(message.Usage)
	s.stopReason = message.StopReason
	for i, block := range message.Content {
		if err := s.addBlock(i, block); err != nil {
			return err
		}
		if err := s.closeBlock(i); err != nil {
			return err
		}
	}
	s.ended = true
	return s.start()
}
func (s *responseStream) finish() {
	status, event := "completed", "response.completed"
	if s.stopReason == "max_tokens" {
		status, event = "incomplete", "response.incomplete"
	}
	if s.stream {
		_ = s.emit(event, map[string]any{"response": s.response(status)})
	} else {
		writeJSON(s.w, 200, s.response(status))
	}
}

func (s *responseStream) summaryText() string {
	var summary strings.Builder
	for _, block := range s.blocks {
		if block.kind != "text" && block.kind != "thinking" {
			continue
		}
		text := strings.TrimSpace(block.text.String())
		if text == "" {
			continue
		}
		if summary.Len() > 0 {
			summary.WriteByte('\n')
		}
		summary.WriteString(text)
	}
	return summary.String()
}

func (s *responseStream) writeCompaction(summary string) error {
	s.suppress = false
	s.blocks = nil
	item := map[string]any{"id": randomID("cmp_"), "type": "compaction", "encrypted_content": encodeCompactionSummary(summary)}
	if !s.stream {
		response := s.response("completed")
		response["output"] = []any{item}
		writeJSON(s.w, 200, response)
		return nil
	}
	if err := s.emit("response.created", map[string]any{"response": s.response("in_progress")}); err != nil {
		return err
	}
	if err := s.emit("response.output_item.added", map[string]any{"output_index": 0, "item": item}); err != nil {
		return err
	}
	if err := s.emit("response.output_item.done", map[string]any{"output_index": 0, "item": item}); err != nil {
		return err
	}
	response := s.response("completed")
	response["output"] = []any{item}
	return s.emit("response.completed", map[string]any{"response": response})
}

func (s *responseStream) fail(err error) {
	s.suppress = false
	if s.stream && s.started {
		response := s.response("failed")
		response["error"] = map[string]any{"code": "upstream_stream_error", "message": err.Error()}
		_ = s.emit("response.failed", map[string]any{"response": response})
	} else {
		writeError(s.w, 502, "api_error", err.Error(), "upstream_stream_error")
	}
}
