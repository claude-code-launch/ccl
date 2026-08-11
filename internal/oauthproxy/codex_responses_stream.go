package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const codexResponsesMaxSSEEventBytes = 16 << 20

type codexFunctionCall struct {
	callID    string
	name      string
	arguments strings.Builder
	emitted   bool
}

type codexResponsesStreamState struct {
	assembler     *anthropicResponseAssembler
	functions     map[string]*codexFunctionCall
	textDeltaSeen bool
	reasoningSeen bool
	terminalSeen  bool
}

func processCodexResponsesStream(reader io.Reader, assembler *anthropicResponseAssembler) error {
	state := &codexResponsesStreamState{
		assembler: assembler,
		functions: make(map[string]*codexFunctionCall),
	}
	if err := readCodexSSE(reader, state.process); err != nil {
		return err
	}
	if !assembler.started {
		if err := assembler.start(); err != nil {
			return err
		}
	}
	if !state.terminalSeen {
		return assembler.finish()
	}
	return nil
}

func readCodexSSE(reader io.Reader, consume func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), codexResponsesMaxSSEEventBytes)
	var data strings.Builder
	var plain strings.Builder
	sawData := false
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
			sawData = true
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		} else if !sawData && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") &&
			!strings.HasPrefix(line, "retry:") && !strings.HasPrefix(line, ":") {
			plain.WriteString(line)
			plain.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex Responses event stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if sawData || strings.TrimSpace(plain.String()) == "" {
		return nil
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(plain.String())), &response); err != nil {
		return fmt.Errorf("decode non-streaming Codex Responses payload: %w", err)
	}
	if eventType := stringValue(response["type"]); eventType == "" || (!strings.HasPrefix(eventType, "response.") && eventType != "error") {
		response = map[string]any{"type": "response.completed", "response": response}
	}
	payload, _ := json.Marshal(response)
	return consume(payload)
}

func (s *codexResponsesStreamState) process(payload []byte) error {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode Codex Responses event: %w", err)
	}
	if !s.assembler.started {
		if response := mapValue(event["response"]); response != nil {
			if id := stringValue(response["id"]); id != "" {
				s.assembler.messageID = id
			}
		}
		if err := s.assembler.start(); err != nil {
			return err
		}
	}
	eventType := stringValue(event["type"])
	switch eventType {
	case "response.created", "response.in_progress":
		return nil
	case "response.reasoning_summary_part.added":
		if s.assembler.activeType == "thinking" {
			return s.assembler.addThinking("\n\n")
		}
	case "response.reasoning_summary_text.delta":
		s.reasoningSeen = true
		return s.assembler.addThinking(stringValue(event["delta"]))
	case "response.reasoning_summary_text.done":
		if !s.reasoningSeen {
			s.reasoningSeen = true
			return s.assembler.addThinking(stringValue(event["text"]))
		}
	case "response.reasoning_summary_part.done":
		if !s.reasoningSeen {
			part := mapValue(event["part"])
			if text := stringValue(part["text"]); text != "" {
				s.reasoningSeen = true
				return s.assembler.addThinking(text)
			}
		}
	case "response.output_text.delta", "response.refusal.delta":
		s.textDeltaSeen = true
		return s.assembler.addText(stringValue(event["delta"]))
	case "response.function_call_arguments.delta":
		call := s.functionForEvent(event)
		call.arguments.WriteString(stringValue(event["delta"]))
	case "response.function_call_arguments.done":
		call := s.functionForEvent(event)
		if arguments := stringValue(event["arguments"]); arguments != "" {
			call.arguments.Reset()
			call.arguments.WriteString(arguments)
		}
	case "response.output_item.added":
		item := mapValue(event["item"])
		if stringValue(item["type"]) == "reasoning" {
			s.reasoningSeen = false
		} else if stringValue(item["type"]) == "function_call" {
			s.updateFunction(event, item)
		}
	case "response.output_item.done":
		return s.processOutputItem(event)
	case "response.completed", "response.incomplete", "response.failed":
		return s.processTerminal(eventType, mapValue(event["response"]))
	case "error":
		return fmt.Errorf("Codex Responses stream error: %s", codexEventErrorMessage(event))
	}
	return nil
}

func (s *codexResponsesStreamState) processOutputItem(event map[string]any) error {
	item := mapValue(event["item"])
	switch stringValue(item["type"]) {
	case "message":
		if s.textDeltaSeen {
			return nil
		}
		for _, part := range sliceValue(item["content"]) {
			content := mapValue(part)
			if kind := stringValue(content["type"]); kind == "output_text" || kind == "refusal" {
				if text := stringValue(content["text"]); text != "" {
					s.textDeltaSeen = true
					if err := s.assembler.addText(text); err != nil {
						return err
					}
				}
			}
		}
	case "reasoning":
		if signature := stringValue(item["encrypted_content"]); signature != "" && s.assembler.activeType == "thinking" {
			s.assembler.blocks[s.assembler.activeIndex].Signature = signature
			return s.assembler.closeActive()
		}
	case "function_call":
		call := s.updateFunction(event, item)
		return s.emitFunction(call)
	}
	return nil
}

func (s *codexResponsesStreamState) processTerminal(eventType string, response map[string]any) error {
	if eventType == "response.failed" {
		if message := codexEventErrorMessage(response); message != "" {
			return fmt.Errorf("Codex Responses failed: %s", message)
		}
		return fmt.Errorf("Codex Responses failed")
	}
	// Some compatible gateways omit output_item.done. Recover any terminal text,
	// reasoning signature, or function call exactly once from response.output.
	for index, rawItem := range sliceValue(response["output"]) {
		item := mapValue(rawItem)
		event := map[string]any{"output_index": float64(index), "item": item}
		if err := s.processOutputItem(event); err != nil {
			return err
		}
	}
	usage := mapValue(response["usage"])
	if value := intValue(usage["input_tokens"]); value > 0 {
		s.assembler.contextTokens = value
	}
	if value := intValue(usage["output_tokens"]); value > 0 {
		s.assembler.outputTokens = value
	}
	if details := mapValue(usage["input_tokens_details"]); details != nil {
		s.assembler.cacheReadTokens = intValue(details["cached_tokens"])
		s.assembler.cacheWriteTokens = intValue(details["cache_write_tokens"])
	}
	if s.assembler.cacheWriteTokens == 0 {
		s.assembler.cacheWriteTokens = intValue(usage["cache_write_tokens"])
	}
	if eventType == "response.incomplete" {
		details := mapValue(response["incomplete_details"])
		switch stringValue(details["reason"]) {
		case "max_output_tokens", "max_tokens":
			s.assembler.stopReason = "max_tokens"
		case "content_filter":
			s.assembler.stopReason = "refusal"
		default:
			s.assembler.stopReason = "end_turn"
		}
	}
	s.terminalSeen = true
	return s.assembler.finish()
}

func (s *codexResponsesStreamState) functionForEvent(event map[string]any) *codexFunctionCall {
	key := codexFunctionKey(event, nil)
	if call := s.functions[key]; call != nil {
		return call
	}
	call := &codexFunctionCall{}
	s.functions[key] = call
	return call
}

func (s *codexResponsesStreamState) updateFunction(event, item map[string]any) *codexFunctionCall {
	key := codexFunctionKey(event, item)
	call := s.functions[key]
	if call == nil {
		call = s.functionForEvent(event)
		if existingKey := codexFunctionKey(event, nil); existingKey != key {
			// Keep the output-index alias: argument delta events commonly carry
			// only output_index after output_item.added introduced the item ID.
			s.functions[key] = call
		}
	}
	if value := stringValue(item["call_id"]); value != "" {
		call.callID = value
	}
	if value := stringValue(item["name"]); value != "" {
		call.name = value
	}
	if arguments := stringValue(item["arguments"]); arguments != "" {
		call.arguments.Reset()
		call.arguments.WriteString(arguments)
	}
	return call
}

func (s *codexResponsesStreamState) emitFunction(call *codexFunctionCall) error {
	if call == nil || call.emitted {
		return nil
	}
	call.emitted = true
	if call.callID == "" {
		call.callID = "call_" + strings.ReplaceAll(uuidString(), "-", "")
	}
	arguments := call.arguments.String()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	return s.assembler.addToolUse(call.callID, call.name, arguments)
}

func codexFunctionKey(event, item map[string]any) string {
	if item != nil {
		for _, key := range []string{"id", "call_id"} {
			if value := stringValue(item[key]); value != "" {
				return key + ":" + value
			}
		}
	}
	if value := stringValue(event["item_id"]); value != "" {
		return "id:" + value
	}
	return fmt.Sprintf("index:%d", intValue(event["output_index"]))
}

func codexEventErrorMessage(value map[string]any) string {
	if value == nil {
		return ""
	}
	if message := stringValue(value["message"]); message != "" {
		return message
	}
	if nested := mapValue(value["error"]); nested != nil {
		if message := stringValue(nested["message"]); message != "" {
			return message
		}
	}
	return ""
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func sliceValue(value any) []any {
	result, _ := value.([]any)
	return result
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}
