package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// chatToolCall accumulates one streamed OpenAI tool_call across its argument
// deltas, mirroring codexFunctionCall. Arguments arrive as partial JSON
// fragments and are joined before emission.
type chatToolCall struct {
	index     int
	id        string
	name      string
	arguments strings.Builder
	emitted   bool
}

type chatCompletionsStreamState struct {
	assembler *anthropicResponseAssembler
	tools     map[string]*chatToolCall
	usage     map[string]any
}

// processChatCompletionsStream converts an OpenAI Chat Completions SSE stream
// into Anthropic Messages SSE, feeding the shared assembler.
func processChatCompletionsStream(reader io.Reader, assembler *anthropicResponseAssembler) error {
	state := &chatCompletionsStreamState{
		assembler: assembler,
		tools:     make(map[string]*chatToolCall),
	}
	if err := readChatCompletionsSSE(reader, state.process); err != nil {
		return err
	}
	if !assembler.started {
		if err := assembler.start(); err != nil {
			return err
		}
	}
	if err := state.emitTools(); err != nil {
		return err
	}
	// The upstream usage is authoritative and arrives in the final chunk, so it
	// must be applied after tool emission (which still counts tokens locally).
	chatApplyUsage(assembler, state.usage)
	return assembler.finish()
}

// readChatCompletionsSSE reads OpenAI `data:` lines and hands each payload to
// consume. `[DONE]` and blank payloads are skipped. Unlike the Codex reader it
// does not attempt a non-streaming JSON fallback: a non-streaming response is
// handled by processChatCompletionsNonStream on the caller's side.
func readChatCompletionsSSE(reader io.Reader, consume func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), codexResponsesMaxSSEEventBytes)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		return consume([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read OpenAI Chat Completions event stream: %w", err)
	}
	return flush()
}

func (s *chatCompletionsStreamState) process(payload []byte) error {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode OpenAI Chat Completions chunk: %w", err)
	}
	if !s.assembler.started {
		if err := s.assembler.start(); err != nil {
			return err
		}
	}
	if usage := mapValue(event["usage"]); usage != nil {
		s.usage = usage
	}
	choices := sliceValue(event["choices"])
	if len(choices) == 0 {
		// The final chunk can carry only usage when stream_options.include_usage
		// is set, with an empty choices array.
		return nil
	}
	choice := mapValue(choices[0])
	if delta := mapValue(choice["delta"]); delta != nil {
		if text := stringValue(delta["content"]); text != "" {
			if err := s.assembler.addText(text); err != nil {
				return err
			}
		}
		if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
			if err := s.assembler.addThinking(reasoning); err != nil {
				return err
			}
		}
		for _, rawCall := range sliceValue(delta["tool_calls"]) {
			s.accumulateToolCall(mapValue(rawCall))
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish != "" {
		s.assembler.stopReason = chatStopReason(finish)
	}
	return nil
}

func (s *chatCompletionsStreamState) accumulateToolCall(raw map[string]any) {
	index := intValue(raw["index"])
	key := fmt.Sprintf("%d", index)
	call := s.tools[key]
	if call == nil {
		call = &chatToolCall{index: index}
		s.tools[key] = call
	}
	if id := stringValue(raw["id"]); id != "" {
		call.id = id
	}
	if function := mapValue(raw["function"]); function != nil {
		if name := stringValue(function["name"]); name != "" {
			call.name = name
		}
		if arguments := stringValue(function["arguments"]); arguments != "" {
			call.arguments.WriteString(arguments)
		}
	}
}

// emitTools flushes every accumulated tool call in index order, synthesizing
// missing ids and names so Claude Code always receives a well-formed tool_use
// block.
func (s *chatCompletionsStreamState) emitTools() error {
	indices := make([]int, 0, len(s.tools))
	for _, call := range s.tools {
		indices = append(indices, call.index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := s.tools[fmt.Sprintf("%d", index)]
		if call == nil || call.emitted {
			continue
		}
		call.emitted = true
		id := call.id
		if strings.TrimSpace(id) == "" {
			id = "call_" + strings.ReplaceAll(uuidString(), "-", "")
		}
		name := call.name
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("tool_%d", index)
		}
		arguments := call.arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if err := s.assembler.addToolUse(id, name, arguments); err != nil {
			return fmt.Errorf("tool %s returned invalid JSON input: %w", name, err)
		}
	}
	return nil
}

// processChatCompletionsNonStream converts a complete OpenAI Chat Completions
// JSON body into an Anthropic Messages response via the shared assembler.
func processChatCompletionsNonStream(body []byte, assembler *anthropicResponseAssembler) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode OpenAI Chat Completions response: %w", err)
	}
	choices := sliceValue(payload["choices"])
	if len(choices) == 0 {
		return fmt.Errorf("OpenAI Chat Completions response has no choices")
	}
	if err := assembler.start(); err != nil {
		return err
	}
	choice := mapValue(choices[0])
	message := mapValue(choice["message"])
	// reasoning_content precedes content, matching Anthropic block order.
	if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
		if err := assembler.addThinking(reasoning); err != nil {
			return err
		}
	}
	if err := chatAddContent(assembler, message["content"]); err != nil {
		return err
	}
	for index, rawCall := range sliceValue(message["tool_calls"]) {
		call := mapValue(rawCall)
		function := mapValue(call["function"])
		name := stringValue(function["name"])
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("tool_%d", index)
		}
		id := stringValue(call["id"])
		if strings.TrimSpace(id) == "" {
			id = "call_" + strings.ReplaceAll(uuidString(), "-", "")
		}
		arguments := stringValue(function["arguments"])
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if err := assembler.addToolUse(id, name, arguments); err != nil {
			return fmt.Errorf("tool %s returned invalid JSON input: %w", name, err)
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish != "" {
		assembler.stopReason = chatStopReason(finish)
	}
	chatApplyUsage(assembler, mapValue(payload["usage"]))
	return assembler.finish()
}

// chatAddContent feeds an OpenAI assistant content field, which is a plain
// string for text-only responses or an array of text/image parts for
// multimodal ones.
func chatAddContent(assembler *anthropicResponseAssembler, content any) error {
	switch typed := content.(type) {
	case string:
		if typed != "" {
			return assembler.addText(typed)
		}
	case []any:
		for _, part := range typed {
			block := mapValue(part)
			if text := stringValue(block["text"]); text != "" {
				if err := assembler.addText(text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// chatApplyUsage records the OpenAI usage object onto the assembler, mapping
// prompt_tokens/completion_tokens and cached prompt tokens to their Anthropic
// usage fields.
func chatApplyUsage(assembler *anthropicResponseAssembler, usage map[string]any) {
	if usage == nil {
		return
	}
	if value := intValue(usage["prompt_tokens"]); value > 0 {
		assembler.contextTokens = value
	}
	if value := intValue(usage["completion_tokens"]); value > 0 {
		assembler.outputTokens = value
	}
	if details := mapValue(usage["prompt_tokens_details"]); details != nil {
		assembler.cacheReadTokens = intValue(details["cached_tokens"])
	}
}

// chatStopReason maps an OpenAI finish_reason onto the Anthropic stop_reason.
func chatStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}
