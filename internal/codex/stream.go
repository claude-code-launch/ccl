package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const bridgeResponseLimit = 64 << 20

type bridgeEvents struct {
	writer   http.ResponseWriter
	sequence int
}

func (s *bridgeEvents) emit(event string, fields map[string]any) error {
	fields["type"] = event
	fields["sequence_number"] = s.sequence
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if s.sequence == 0 {
		s.writer.Header().Set("Content-Type", "text/event-stream")
		s.writer.Header().Set("Cache-Control", "no-cache")
	}
	s.sequence++
	if _, err := fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return http.NewResponseController(s.writer).Flush()
}

// relayBridgeStream keeps the Responses events produced by the runtime. Only
// the public model alias and the optional transcript migration notice are
// added at the gateway. The terminal response supplies the history cache.
func relayBridgeStream(w http.ResponseWriter, body io.Reader, model, notice string, record func(map[string]any)) {
	events := bridgeEvents{writer: w}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), bridgeResponseLimit)
	var noticeItem map[string]any
	if notice != "" {
		noticeItem = migrationNoticeItem(notice)
	}
	for scanner.Scan() {
		line, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok || strings.TrimSpace(line) == "[DONE]" {
			continue
		}
		var event map[string]any
		if decodeBridgeJSON([]byte(line), &event) != nil {
			break
		}
		kind, _ := event["type"].(string)
		if kind == "" {
			break
		}
		response, _ := event["response"].(map[string]any)
		if response != nil {
			response["model"] = model
		}
		terminal := kind == "response.completed" || kind == "response.incomplete"
		if noticeItem != nil {
			index, indexed := event["output_index"].(json.Number)
			if (indexed || terminal) && notice != "" {
				if events.writeNotice(noticeItem) != nil {
					return
				}
				notice = ""
			}
			if indexed {
				n, _ := index.Int64()
				event["output_index"] = n + 1
			}
			if terminal {
				prependNotice(response, noticeItem)
			}
		}
		if terminal {
			if response == nil {
				break
			}
			record(response)
			_ = events.emit(kind, event)
			return
		}
		if events.emit(kind, event) != nil || kind == "response.failed" || kind == "error" {
			return
		}
	}
	const message = "CCL runtime stream ended before a terminal response"
	if events.sequence == 0 {
		writeError(w, http.StatusBadGateway, "api_error", message, "upstream_stream_error")
	} else {
		_ = events.emit("error", map[string]any{"code": "upstream_stream_error", "message": message, "param": nil})
	}
}

func migrationNoticeItem(text string) map[string]any {
	return map[string]any{"id": randomID("msg_"), "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func prependNotice(response, item map[string]any) {
	if response != nil {
		output, _ := response["output"].([]any)
		response["output"] = append([]any{item}, output...)
	}
}

func (s *bridgeEvents) writeNotice(item map[string]any) error {
	id := item["id"]
	added := map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	part := item["content"].([]any)[0].(map[string]any)
	for _, event := range []struct {
		kind   string
		fields map[string]any
	}{
		{"response.output_item.added", map[string]any{"output_index": 0, "item": added}},
		{"response.content_part.added", map[string]any{"output_index": 0, "content_index": 0, "item_id": id,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}},
		{"response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": id, "delta": part["text"]}},
		{"response.output_text.done", map[string]any{"output_index": 0, "content_index": 0, "item_id": id, "text": part["text"]}},
		{"response.content_part.done", map[string]any{"output_index": 0, "content_index": 0, "item_id": id, "part": part}},
		{"response.output_item.done", map[string]any{"output_index": 0, "item": item}},
	} {
		if err := s.emit(event.kind, event.fields); err != nil {
			return err
		}
	}
	return nil
}

func writeBridgeCompaction(w http.ResponseWriter, request responseRequest, id, summary string, usage any) {
	item := map[string]any{"id": randomID("cmp_"), "type": "compaction", "encrypted_content": encodeCompactionSummary(summary)}
	response := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(),
		"model": request.Model, "status": "completed", "output": []any{item}, "usage": usage,
		"error": nil, "incomplete_details": nil, "parallel_tool_calls": false}
	if !request.Stream {
		writeJSON(w, http.StatusOK, response)
		return
	}
	start := map[string]any{"id": id, "object": "response", "created_at": response["created_at"],
		"model": request.Model, "status": "in_progress", "output": []any{}, "usage": nil}
	events := bridgeEvents{writer: w}
	for _, event := range []struct {
		kind   string
		fields map[string]any
	}{
		{"response.created", map[string]any{"response": start}},
		{"response.output_item.added", map[string]any{"output_index": 0, "item": item}},
		{"response.output_item.done", map[string]any{"output_index": 0, "item": item}},
		{"response.completed", map[string]any{"response": response}},
	} {
		if events.emit(event.kind, event.fields) != nil {
			return
		}
	}
}
