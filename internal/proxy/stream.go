package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

// StreamTransformer coordinates translating a stream of OpenAI chunks into Anthropic SSE chunks.
type StreamTransformer struct {
	sentMessageStart  bool
	currentBlockType  string
	currentBlockIdx   int
	messageID         string
	model             string
	tools             map[int]*chatToolState
	sentMessageStop   bool
	pendingStopReason string
	pendingUsage      map[string]any
	hasPendingDelta   bool
}

type chatToolState struct {
	index       int
	id          string
	name        string
	started     bool
	pendingArgs strings.Builder
}

type ResponsesStreamTransformer struct {
	sentMessageStart    bool
	currentBlockType    string
	currentBlockIdx     int
	sentMessageStop     bool
	messageID           string
	model               string
	hasToolUse          bool
	usage               map[string]any
	tools               map[int]*responsesToolState
	textOutputSeen      map[int]bool
	reasoningOutputSeen map[int]bool
}

type responsesToolState struct {
	index       int
	id          string
	name        string
	started     bool
	pendingArgs strings.Builder
}

// TranslateChunk transforms a single raw line of "data: {...}" from OpenAI into one or more Anthropic SSE events.
func (st *StreamTransformer) TranslateChunk(line string) ([]string, error) {
	line = strings.TrimPrefix(line, "data: ")
	line = strings.TrimSpace(line)

	if line == "" || line == "[DONE]" {
		if line == "[DONE]" {
			return st.Finish(), nil
		}
		return nil, nil
	}

	var chunk protocol.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(line), &chunk); err != nil {
		// Non-JSON or parsing error, skip or pass through
		return nil, nil
	}

	if chunk.Usage != nil {
		st.pendingUsage = chatUsage(chunk.Usage)
	}
	if st.messageID == "" {
		st.messageID = chunk.ID
	}
	if st.model == "" {
		st.model = chunk.Model
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}

	var events []string
	choice := chunk.Choices[0]
	delta := choice.Delta

	if delta.ReasoningContent != "" {
		events = append(events, st.ensureChatThinkingBlock()...)
		events = append(events, anthEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.currentBlockIdx,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": delta.ReasoningContent,
			},
		}))
	}

	if delta.Content != "" {
		events = append(events, st.ensureChatTextBlock()...)
		events = append(events, anthEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.currentBlockIdx,
			"delta": map[string]any{
				"type": "text_delta",
				"text": delta.Content,
			},
		}))
	}

	if len(delta.ToolCalls) > 0 {
		for _, tc := range delta.ToolCalls {
			events = append(events, st.ensureChatToolBlock(tc.Index, &tc)...)
			if tc.Function.Arguments != "" {
				st.tools[tc.Index].pendingArgs.WriteString(tc.Function.Arguments)
				events = append(events, anthEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.tools[tc.Index].index,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": tc.Function.Arguments,
					},
				}))
			}
		}
	}

	if choice.FinishReason != nil {
		if st.hasPendingDelta {
			st.pendingStopReason = mapChatStopReason(*choice.FinishReason)
			return events, nil
		}
		events = append(events, st.closeCurrentChatBlock()...)
		for _, state := range st.sortedChatTools() {
			if state.started {
				events = append(events, anthEvent("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": state.index,
				}))
			}
		}
		st.pendingStopReason = mapChatStopReason(*choice.FinishReason)
		st.hasPendingDelta = true
	}

	return events, nil
}

func (st *StreamTransformer) Finish() []string {
	if st.sentMessageStop {
		return nil
	}
	events := st.ensureChatMessageStart()
	events = append(events, st.closeCurrentChatBlock()...)
	stopReason := st.pendingStopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := st.pendingUsage
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	events = append(events,
		anthEvent("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": usage,
		}),
		anthEvent("message_stop", map[string]any{"type": "message_stop"}),
	)
	st.sentMessageStop = true
	return events
}

func (st *StreamTransformer) ensureChatMessageStart() []string {
	if st.sentMessageStart {
		return nil
	}
	st.sentMessageStart = true
	return []string{anthEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            st.messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         st.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})}
}

func (st *StreamTransformer) ensureChatThinkingBlock() []string {
	events := st.ensureChatMessageStart()
	if st.currentBlockType == "thinking" {
		return events
	}
	events = append(events, st.closeCurrentChatBlock()...)
	st.currentBlockType = "thinking"
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.currentBlockIdx,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	}))
	return events
}

func (st *StreamTransformer) ensureChatTextBlock() []string {
	events := st.ensureChatMessageStart()
	if st.currentBlockType == "text" {
		return events
	}
	events = append(events, st.closeCurrentChatBlock()...)
	st.currentBlockType = "text"
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.currentBlockIdx,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}))
	return events
}

func (st *StreamTransformer) ensureChatToolBlock(openAIIndex int, tc *protocol.OpenAIToolCall) []string {
	events := st.ensureChatMessageStart()
	events = append(events, st.closeCurrentChatBlock()...)
	if st.tools == nil {
		st.tools = make(map[int]*chatToolState)
	}
	state := st.tools[openAIIndex]
	if state == nil {
		state = &chatToolState{
			index: st.currentBlockIdx,
			id:    fmt.Sprintf("tool_call_%d", openAIIndex),
			name:  "unknown_tool",
		}
		st.currentBlockIdx++
		st.tools[openAIIndex] = state
	}
	if tc != nil {
		if tc.ID != "" {
			state.id = tc.ID
		}
		if tc.Function.Name != "" {
			state.name = tc.Function.Name
		}
	}
	if state.started {
		return events
	}
	state.started = true
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": state.index,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   state.id,
			"name": state.name,
		},
	}))
	return events
}

func (st *StreamTransformer) closeCurrentChatBlock() []string {
	if st.currentBlockType == "" {
		return nil
	}
	event := anthEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.currentBlockIdx,
	})
	st.currentBlockType = ""
	st.currentBlockIdx++
	return []string{event}
}

func (st *StreamTransformer) sortedChatTools() []*chatToolState {
	if len(st.tools) == 0 {
		return nil
	}
	states := make([]*chatToolState, 0, len(st.tools))
	for _, state := range st.tools {
		states = append(states, state)
	}
	for i := 1; i < len(states); i++ {
		for j := i; j > 0 && states[j-1].index > states[j].index; j-- {
			states[j-1], states[j] = states[j], states[j-1]
		}
	}
	return states
}

func mapChatStopReason(finishReason string) string {
	switch finishReason {
	case "stop":
		return "end_turn"
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func chatUsage(usage *protocol.OpenAIUsage) map[string]any {
	if usage == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
	}
}

func (st *ResponsesStreamTransformer) TranslateBlock(block string) ([]string, error) {
	eventName, data := parseSSEBlock(block)
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	if data == "[DONE]" {
		return st.finish("end_turn"), nil
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil, nil
	}
	if st.tools == nil {
		st.tools = make(map[int]*responsesToolState)
	}
	if st.textOutputSeen == nil {
		st.textOutputSeen = make(map[int]bool)
	}
	if st.reasoningOutputSeen == nil {
		st.reasoningOutputSeen = make(map[int]bool)
	}

	// Many OpenAI-compatible gateways omit the SSE "event:" line and only put
	// the event name in JSON "type". Prefer the explicit event line when present.
	if eventName == "" {
		eventName = stringFromAny(payload["type"])
	}

	st.captureResponseMetadata(payload)
	if response := mapFromAny(payload["response"]); len(response) > 0 {
		st.captureResponseMetadata(response)
	}

	var events []string
	switch eventName {
	case "response.output_item.added":
		item := mapFromAny(payload["item"])
		events = append(events, st.handleResponsesOutputItem(payload, item, false)...)
	case "response.output_item.done":
		item := mapFromAny(payload["item"])
		events = append(events, st.handleResponsesOutputItem(payload, item, true)...)
	case "response.output_text.delta", "response.content_part.delta":
		outputIndex := intFromAny(payload["output_index"])
		delta := stringFromAny(payload["delta"])
		// content_part.delta may nest text under delta.text
		if delta == "" {
			if nested := mapFromAny(payload["delta"]); len(nested) > 0 {
				delta = stringFromAny(nested["text"])
			}
		}
		if delta != "" {
			st.textOutputSeen[outputIndex] = true
			events = append(events, st.ensureResponsesTextBlock()...)
			events = append(events, anthEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.currentBlockIdx,
				"delta": map[string]any{
					"type": "text_delta",
					"text": delta,
				},
			}))
		}
	case "response.output_text.done":
		outputIndex := intFromAny(payload["output_index"])
		text := stringFromAny(payload["text"])
		if text != "" && !st.textOutputSeen[outputIndex] {
			st.textOutputSeen[outputIndex] = true
			events = append(events, st.responsesTextDelta(text)...)
		}
	case "response.content_part.done":
		outputIndex := intFromAny(payload["output_index"])
		part := mapFromAny(payload["part"])
		partType := stringFromAny(part["type"])
		text := stringFromAny(part["text"])
		if text != "" && !st.textOutputSeen[outputIndex] &&
			(partType == "output_text" || partType == "text" || partType == "refusal" || partType == "") {
			st.textOutputSeen[outputIndex] = true
			events = append(events, st.responsesTextDelta(text)...)
		}
	case "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.delta":
		outputIndex := intFromAny(payload["output_index"])
		delta := stringFromAny(payload["delta"])
		if delta == "" {
			if nested := mapFromAny(payload["delta"]); len(nested) > 0 {
				delta = stringFromAny(nested["text"])
			}
		}
		if delta != "" {
			st.reasoningOutputSeen[outputIndex] = true
			events = append(events, st.ensureResponsesThinkingBlock()...)
			events = append(events, anthEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.currentBlockIdx,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": delta,
				},
			}))
		}
	case "response.function_call_arguments.delta":
		outputIndex := intFromAny(payload["output_index"])
		delta := stringFromAny(payload["delta"])
		if delta != "" {
			events = append(events, st.ensureResponsesToolBlock(outputIndex, nil)...)
			st.tools[outputIndex].pendingArgs.WriteString(delta)
			events = append(events, anthEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.tools[outputIndex].index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": delta,
				},
			}))
		}
	case "response.function_call_arguments.done":
		outputIndex := intFromAny(payload["output_index"])
		args := stringFromAny(payload["arguments"])
		if args != "" {
			events = append(events, st.ensureResponsesToolBlock(outputIndex, nil)...)
			if st.tools[outputIndex].pendingArgs.Len() == 0 {
				events = append(events, anthEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.tools[outputIndex].index,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": args,
					},
				}))
			}
		}
	case "response.completed":
		response := mapFromAny(payload["response"])
		st.captureUsage(response)
		events = append(events, st.handleResponsesFinalOutput(response)...)
		if !st.anyResponsesTextOutputSeen() && !st.hasToolUse {
			events = append(events, st.responsesErrorWithFallback(payload, "OpenAI Responses stream completed without assistant output")...)
			break
		}
		stopReason := "end_turn"
		if st.hasToolUse {
			stopReason = "tool_use"
		} else if status := stringFromAny(response["status"]); status == "incomplete" || status == "failed" {
			stopReason = "max_tokens"
		}
		events = append(events, st.finish(stopReason)...)
	case "response.incomplete":
		response := mapFromAny(payload["response"])
		st.captureUsage(response)
		events = append(events, st.handleResponsesFinalOutput(response)...)
		if !st.anyResponsesTextOutputSeen() && !st.hasToolUse {
			events = append(events, st.responsesErrorWithFallback(payload, "OpenAI Responses stream ended incomplete without assistant output")...)
			break
		}
		stopReason := "max_tokens"
		if st.hasToolUse {
			stopReason = "tool_use"
		}
		events = append(events, st.finish(stopReason)...)
	case "response.failed", "error":
		events = append(events, st.responsesErrorWithFallback(payload, "OpenAI Responses stream failed")...)
	}

	return events, nil
}

func (st *ResponsesStreamTransformer) handleResponsesOutputItem(payload, item map[string]any, done bool) []string {
	if len(item) == 0 {
		return nil
	}
	st.captureResponseMetadata(item)

	switch stringFromAny(item["type"]) {
	case "message":
		events := st.ensureResponsesMessageStart()
		// Some gateways only emit full text on output_item.done (no deltas).
		outputIndex := intFromAny(payload["output_index"])
		if done && !st.textOutputSeen[outputIndex] {
			for _, partAny := range sliceFromAny(item["content"]) {
				part := mapFromAny(partAny)
				text := stringFromAny(part["text"])
				partType := stringFromAny(part["type"])
				if text == "" {
					continue
				}
				if partType == "output_text" || partType == "text" || partType == "refusal" || partType == "" {
					st.textOutputSeen[outputIndex] = true
					events = append(events, st.responsesTextDelta(text)...)
				}
			}
		}
		return events
	case "reasoning":
		events := st.ensureResponsesMessageStart()
		outputIndex := intFromAny(payload["output_index"])
		if done && !st.reasoningOutputSeen[outputIndex] {
			for _, partAny := range sliceFromAny(item["summary"]) {
				part := mapFromAny(partAny)
				if text := stringFromAny(part["text"]); text != "" {
					st.reasoningOutputSeen[outputIndex] = true
					events = append(events, st.ensureResponsesThinkingBlock()...)
					events = append(events, anthEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": st.currentBlockIdx,
						"delta": map[string]any{
							"type":     "thinking_delta",
							"thinking": text,
						},
					}))
				}
			}
		}
		return events
	case "function_call":
		outputIndex := intFromAny(payload["output_index"])
		events := st.ensureResponsesToolBlock(outputIndex, item)
		if done {
			if args := stringFromAny(item["arguments"]); args != "" {
				if st.tools[outputIndex].pendingArgs.Len() == 0 {
					events = append(events, anthEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": st.tools[outputIndex].index,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": args,
						},
					}))
				}
			}
		}
		return events
	}
	return nil
}

func (st *ResponsesStreamTransformer) handleResponsesFinalOutput(response map[string]any) []string {
	var events []string
	output := sliceFromAny(response["output"])
	for outputIndex, itemAny := range output {
		item := mapFromAny(itemAny)
		events = append(events, st.handleResponsesOutputItem(
			map[string]any{"output_index": outputIndex},
			item,
			true,
		)...)
	}

	// A few compatible gateways expose only the SDK-style flattened text on
	// the completed response. Preserve it when no indexed text was emitted.
	if text := stringFromAny(response["output_text"]); text != "" && !st.anyResponsesTextOutputSeen() {
		st.textOutputSeen[0] = true
		events = append(events, st.responsesTextDelta(text)...)
	}
	return events
}

func (st *ResponsesStreamTransformer) responsesTextDelta(text string) []string {
	events := st.ensureResponsesTextBlock()
	return append(events, anthEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.currentBlockIdx,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	}))
}

func (st *ResponsesStreamTransformer) anyResponsesTextOutputSeen() bool {
	for _, seen := range st.textOutputSeen {
		if seen {
			return true
		}
	}
	return false
}

func (st *ResponsesStreamTransformer) responsesErrorWithFallback(payload map[string]any, fallback string) []string {
	if st.sentMessageStop {
		return nil
	}

	detail := mapFromAny(payload["error"])
	response := mapFromAny(payload["response"])
	if len(detail) == 0 {
		detail = mapFromAny(response["error"])
	}
	message := stringFromAny(detail["message"])
	if message == "" {
		message = stringFromAny(payload["message"])
	}
	if message == "" {
		message = fallback
	}

	// An Anthropic stream may terminate with an error event after HTTP headers
	// have already been sent. Mark the stream terminal so the EOF fallback does
	// not append an empty successful assistant message after the error.
	st.sentMessageStop = true
	return []string{anthEvent("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": message,
		},
	})}
}

func sliceFromAny(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func (st *ResponsesStreamTransformer) ensureResponsesMessageStart() []string {
	if st.sentMessageStart {
		return nil
	}
	st.sentMessageStart = true
	return []string{anthEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            st.messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         st.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})}
}

func (st *ResponsesStreamTransformer) ensureResponsesTextBlock() []string {
	events := st.ensureResponsesMessageStart()
	if st.currentBlockType == "text" {
		return events
	}
	events = append(events, st.closeCurrentResponsesBlock()...)
	st.currentBlockType = "text"
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.currentBlockIdx,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}))
	return events
}

func (st *ResponsesStreamTransformer) ensureResponsesThinkingBlock() []string {
	events := st.ensureResponsesMessageStart()
	if st.currentBlockType == "thinking" {
		return events
	}
	events = append(events, st.closeCurrentResponsesBlock()...)
	st.currentBlockType = "thinking"
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.currentBlockIdx,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	}))
	return events
}

func (st *ResponsesStreamTransformer) ensureResponsesToolBlock(outputIndex int, item map[string]any) []string {
	events := st.ensureResponsesMessageStart()
	st.hasToolUse = true

	state := st.tools[outputIndex]
	if state == nil {
		// Close any open text/thinking block before allocating a tool index.
		events = append(events, st.closeCurrentResponsesBlock()...)
		state = &responsesToolState{
			index: st.currentBlockIdx,
			id:    fmt.Sprintf("call_%d", outputIndex),
			name:  "unknown_tool",
		}
		st.currentBlockIdx++
		st.tools[outputIndex] = state
	} else if !state.started {
		events = append(events, st.closeCurrentResponsesBlock()...)
	}
	if item != nil {
		// Prefer call_id (stable across turns); fall back to item id only while
		// we still hold a synthetic call_* placeholder.
		if id := stringFromAny(item["call_id"]); id != "" {
			state.id = id
		} else if id := stringFromAny(item["id"]); id != "" && strings.HasPrefix(state.id, "call_") {
			state.id = id
		}
		if name := stringFromAny(item["name"]); name != "" {
			state.name = name
		}
	}
	if state.started {
		return events
	}
	state.started = true
	events = append(events, anthEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": state.index,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   state.id,
			"name": state.name,
		},
	}))
	return events
}

func (st *ResponsesStreamTransformer) closeCurrentResponsesBlock() []string {
	if st.currentBlockType == "" {
		return nil
	}
	event := anthEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.currentBlockIdx,
	})
	st.currentBlockType = ""
	st.currentBlockIdx++
	return []string{event}
}

func (st *ResponsesStreamTransformer) finish(stopReason string) []string {
	if st.sentMessageStop {
		return nil
	}
	events := st.ensureResponsesMessageStart()
	events = append(events, st.closeCurrentResponsesBlock()...)
	for _, state := range st.sortedResponsesTools() {
		if state.started {
			events = append(events, anthEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": state.index,
			}))
		}
	}
	usage := st.usage
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	events = append(events,
		anthEvent("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": usage,
		}),
		anthEvent("message_stop", map[string]any{"type": "message_stop"}),
	)
	st.sentMessageStop = true
	return events
}

func (st *ResponsesStreamTransformer) sortedResponsesTools() []*responsesToolState {
	if len(st.tools) == 0 {
		return nil
	}
	states := make([]*responsesToolState, 0, len(st.tools))
	for _, state := range st.tools {
		states = append(states, state)
	}
	for i := 1; i < len(states); i++ {
		for j := i; j > 0 && states[j-1].index > states[j].index; j-- {
			states[j-1], states[j] = states[j], states[j-1]
		}
	}
	return states
}

func (st *ResponsesStreamTransformer) captureResponseMetadata(payload map[string]any) {
	if st.messageID == "" {
		st.messageID = stringFromAny(payload["id"])
	}
	if st.model == "" {
		st.model = stringFromAny(payload["model"])
	}
	st.captureUsage(payload)
}

func (st *ResponsesStreamTransformer) captureUsage(payload map[string]any) {
	usage := mapFromAny(payload["usage"])
	if len(usage) == 0 {
		return
	}
	st.usage = map[string]any{
		"input_tokens":  intFromAny(usage["input_tokens"]),
		"output_tokens": intFromAny(usage["output_tokens"]),
	}
}

func parseSSEBlock(block string) (eventName string, data string) {
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return eventName, strings.Join(dataLines, "\n")
}

func anthEvent(eventName string, payload map[string]any) string {
	data, _ := json.Marshal(payload)
	return "event: " + eventName + "\ndata: " + string(data)
}

func mapFromAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// FormatEvents formats a list of raw event strings back into SSE response payloads.
func FormatEvents(events []string) string {
	if len(events) == 0 {
		return ""
	}
	return strings.Join(events, "\n\n") + "\n\n"
}
