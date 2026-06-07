package proxy

import (
	"encoding/json"
	"strings"

	"github.com/haiboyuwen/cc/internal/protocol"
)

// StreamTransformer coordinates translating a stream of OpenAI chunks into Anthropic SSE chunks.
type StreamTransformer struct {
	sentMessageStart bool
	sentBlockStart   bool
	activeToolID     string
	activeToolName   string
	toolArgsBuilder  strings.Builder
}

// TranslateChunk transforms a single raw line of "data: {...}" from OpenAI into one or more Anthropic SSE events.
func (st *StreamTransformer) TranslateChunk(line string) ([]string, error) {
	line = strings.TrimPrefix(line, "data: ")
	line = strings.TrimSpace(line)

	if line == "" || line == "[DONE]" {
		if line == "[DONE]" {
			// Stream completed, return message stop event
			return []string{
				"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
				"event: message_stop\ndata: " + `{"type":"message_stop"}`,
			}, nil
		}
		return nil, nil
	}

	var chunk protocol.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(line), &chunk); err != nil {
		// Non-JSON or parsing error, skip or pass through
		return nil, nil
	}

	if len(chunk.Choices) == 0 {
		return nil, nil
	}

	var events []string
	choice := chunk.Choices[0]
	delta := choice.Delta

	// 1. If we haven't emitted message_start, emit it now.
	if !st.sentMessageStart {
		st.sentMessageStart = true
		msgStart := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":           chunk.ID,
				"type":         "message",
				"role":         "assistant",
				"content":      []any{},
				"model":        chunk.Model,
				"stop_reason":  nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		}
		data, _ := json.Marshal(msgStart)
		events = append(events, "event: message_start\ndata: "+string(data))
	}

	// 2. Check for Text Content block delta
	if delta.Content != "" {
		if !st.sentBlockStart {
			st.sentBlockStart = true
			blockStart := map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			}
			data, _ := json.Marshal(blockStart)
			events = append(events, "event: content_block_start\ndata: "+string(data))
		}

		blockDelta := map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{
				"type": "text_delta",
				"text": delta.Content,
			},
		}
		data, _ := json.Marshal(blockDelta)
		events = append(events, "event: content_block_delta\ndata: "+string(data))
	}

	// 3. Check for Tool Call delta
	if len(delta.ToolCalls) > 0 {
		tc := delta.ToolCalls[0]

		// Check if a new tool call block started
		if tc.ID != "" && tc.ID != st.activeToolID {
			st.activeToolID = tc.ID
			st.activeToolName = tc.Function.Name
			st.toolArgsBuilder.Reset()

			blockStart := map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": map[string]any{},
				},
			}
			data, _ := json.Marshal(blockStart)
			events = append(events, "event: content_block_start\ndata: "+string(data))
		}

		if tc.Function.Arguments != "" {
			st.toolArgsBuilder.WriteString(tc.Function.Arguments)

			blockDelta := map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type":        "input_json_delta",
					"partial_json": tc.Function.Arguments,
				},
			}
			data, _ := json.Marshal(blockDelta)
			events = append(events, "event: content_block_delta\ndata: "+string(data))
		}
	}

	// 4. Check if we received finish reason
	if choice.FinishReason != nil {
		fr := *choice.FinishReason

		// If a content block was open, close it
		if st.sentBlockStart {
			events = append(events, "event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`)
			st.sentBlockStart = false
		}

		// Handle tool call completion inside stream
		if st.activeToolID != "" {
			events = append(events, "event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`)
			st.activeToolID = ""
			st.activeToolName = ""
			st.toolArgsBuilder.Reset()
		}

		var stopReason string
		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "tool_calls":
			stopReason = "tool_use"
		case "length":
			stopReason = "max_tokens"
		default:
			stopReason = "end_turn"
		}

		msgDelta := map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":  stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": 1,
			},
		}
		data, _ := json.Marshal(msgDelta)
		events = append(events, "event: message_delta\ndata: "+string(data))
		events = append(events, "event: message_stop\ndata: "+`{"type":"message_stop"}`)
	}

	return events, nil
}

// FormatEvents formats a list of raw event strings back into SSE response payloads.
func FormatEvents(events []string) string {
	if len(events) == 0 {
		return ""
	}
	return strings.Join(events, "\n\n") + "\n\n"
}
