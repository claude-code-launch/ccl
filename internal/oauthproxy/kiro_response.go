package oauthproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type kiroResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     *map[string]any `json:"input,omitempty"`
}

type kiroToolAccumulator struct {
	name  string
	input strings.Builder
}

type kiroAnthropicAssembler struct {
	request         *kiroConvertedRequest
	writer          http.ResponseWriter
	flusher         http.Flusher
	messageID       string
	blocks          []kiroResponseBlock
	activeIndex     int
	activeType      string
	outputTokens    int
	contextTokens   int
	hasToolUse      bool
	tools           map[string]*kiroToolAccumulator
	creditUsage     float64
	creditUnit      string
	creditPlural    string
	stopReason      string
	started         bool
	finished        bool
	nativeReasoning bool
	inlineState     string
	inlineBuffer    string
}

func newKiroAnthropicAssembler(request *kiroConvertedRequest, writer http.ResponseWriter) *kiroAnthropicAssembler {
	assembler := &kiroAnthropicAssembler{
		request:     request,
		writer:      writer,
		messageID:   "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		activeIndex: -1,
		tools:       make(map[string]*kiroToolAccumulator),
	}
	if writer != nil {
		assembler.flusher, _ = writer.(http.Flusher)
	}
	return assembler
}

func (a *kiroAnthropicAssembler) start() error {
	if a.started {
		return nil
	}
	a.started = true
	return a.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            a.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         a.request.clientModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                a.request.inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
}

func (a *kiroAnthropicAssembler) process(frame *kiroEventFrame) error {
	messageType := frame.headers[":message-type"]
	switch messageType {
	case "", "event":
		return a.processEvent(frame.headers[":event-type"], frame.payload)
	case "error":
		code := frame.headers[":error-code"]
		return fmt.Errorf("Kiro event stream error %s: %s", code, strings.TrimSpace(string(frame.payload)))
	case "exception":
		kind := frame.headers[":exception-type"]
		if kind == "ContentLengthExceededException" {
			a.stopReason = "max_tokens"
			return nil
		}
		return fmt.Errorf("Kiro event stream exception %s: %s", kind, kiroEventMessage(frame.payload))
	default:
		return fmt.Errorf("unsupported Kiro EventStream message type %q", messageType)
	}
}

func (a *kiroAnthropicAssembler) processEvent(eventType string, payload []byte) error {
	switch eventType {
	case "assistantResponseEvent":
		var event struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.Content != "" {
			return a.addAssistantContent(event.Content)
		}
	case "reasoningContentEvent":
		var event struct {
			Text            string `json:"text"`
			Signature       string `json:"signature"`
			RedactedContent string `json:"redactedContent"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		a.nativeReasoning = true
		if err := a.flushInlineContent(); err != nil {
			return err
		}
		if event.RedactedContent != "" {
			if err := a.closeActive(); err != nil {
				return err
			}
			index := len(a.blocks)
			a.blocks = append(a.blocks, kiroResponseBlock{Type: "redacted_thinking", Data: event.RedactedContent})
			if err := a.emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "redacted_thinking",
					"data": event.RedactedContent,
				},
			}); err != nil {
				return err
			}
			if err := a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
		}
		if event.Text != "" {
			if a.request.thinkingEnabled {
				if err := a.addThinking(event.Text); err != nil {
					return err
				}
			} else if err := a.addText(event.Text); err != nil {
				return err
			}
		}
		if event.Signature != "" && a.activeType == "thinking" && a.activeIndex >= 0 {
			a.blocks[a.activeIndex].Signature = event.Signature
		}
	case "toolUseEvent":
		var event struct {
			Name      string `json:"name"`
			ToolUseID string `json:"toolUseId"`
			Input     string `json:"input"`
			Stop      bool   `json:"stop"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.ToolUseID == "" {
			return fmt.Errorf("Kiro toolUseEvent is missing toolUseId")
		}
		accumulator := a.tools[event.ToolUseID]
		if accumulator == nil {
			accumulator = &kiroToolAccumulator{name: event.Name}
			a.tools[event.ToolUseID] = accumulator
		}
		if event.Name != "" {
			accumulator.name = event.Name
		}
		accumulator.input.WriteString(event.Input)
		if event.Stop {
			delete(a.tools, event.ToolUseID)
			if err := a.flushInlineContent(); err != nil {
				return err
			}
			return a.addToolUse(event.ToolUseID, accumulator.name, accumulator.input.String())
		}
	case "contextUsageEvent":
		var event struct {
			ContextUsagePercentage float64 `json:"contextUsagePercentage"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.ContextUsagePercentage > 0 {
			a.contextTokens = int(event.ContextUsagePercentage / 100 * float64(kiroContextWindow(a.request.model)))
			if event.ContextUsagePercentage >= 100 {
				a.stopReason = "model_context_window_exceeded"
			}
		}
	case "meteringEvent":
		var event struct {
			Unit       string  `json:"unit"`
			UnitPlural string  `json:"unitPlural"`
			Usage      float64 `json:"usage"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		a.creditUsage = event.Usage
		a.creditUnit = event.Unit
		a.creditPlural = event.UnitPlural
	}
	return nil
}

func (a *kiroAnthropicAssembler) addAssistantContent(content string) error {
	if !a.request.thinkingEnabled || a.nativeReasoning || a.inlineState == "done" {
		return a.addText(content)
	}
	a.inlineBuffer += content
	for {
		switch a.inlineState {
		case "thinking":
			if index := strings.Index(a.inlineBuffer, "</thinking>"); index >= 0 {
				if err := a.addThinking(a.inlineBuffer[:index]); err != nil {
					return err
				}
				a.inlineBuffer = a.inlineBuffer[index+len("</thinking>"):]
				a.inlineState = "done"
				if a.inlineBuffer != "" {
					remaining := a.inlineBuffer
					a.inlineBuffer = ""
					return a.addText(remaining)
				}
				return nil
			}
			keep := partialKiroTagSuffix(a.inlineBuffer, "</thinking>")
			safe := a.inlineBuffer[:len(a.inlineBuffer)-keep]
			a.inlineBuffer = a.inlineBuffer[len(a.inlineBuffer)-keep:]
			return a.addThinking(safe)
		default:
			if index := strings.Index(a.inlineBuffer, "<thinking>"); index >= 0 {
				before := a.inlineBuffer[:index]
				if strings.TrimSpace(before) != "" {
					if err := a.addText(before); err != nil {
						return err
					}
				}
				a.inlineBuffer = a.inlineBuffer[index+len("<thinking>"):]
				a.inlineState = "thinking"
				continue
			}
			keep := partialKiroTagSuffix(a.inlineBuffer, "<thinking>")
			safe := a.inlineBuffer[:len(a.inlineBuffer)-keep]
			a.inlineBuffer = a.inlineBuffer[len(a.inlineBuffer)-keep:]
			return a.addText(safe)
		}
	}
}

func (a *kiroAnthropicAssembler) flushInlineContent() error {
	if a.inlineBuffer == "" {
		return nil
	}
	buffer := a.inlineBuffer
	a.inlineBuffer = ""
	if a.inlineState == "thinking" {
		if index := strings.Index(buffer, "</thinking>"); index >= 0 {
			if err := a.addThinking(buffer[:index]); err != nil {
				return err
			}
			a.inlineState = "done"
			return a.addText(buffer[index+len("</thinking>"):])
		}
		return a.addThinking(buffer)
	}
	return a.addText(buffer)
}

func partialKiroTagSuffix(content, tag string) int {
	maximum := len(tag) - 1
	if len(content) < maximum {
		maximum = len(content)
	}
	for size := maximum; size > 0; size-- {
		if strings.HasSuffix(content, tag[:size]) {
			return size
		}
	}
	return 0
}

func (a *kiroAnthropicAssembler) addText(text string) error {
	if text == "" {
		return nil
	}
	index, err := a.ensureBlock("text")
	if err != nil {
		return err
	}
	a.blocks[index].Text += text
	a.outputTokens += estimateKiroTokens(text)
	return a.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (a *kiroAnthropicAssembler) addThinking(thinking string) error {
	if thinking == "" {
		return nil
	}
	index, err := a.ensureBlock("thinking")
	if err != nil {
		return err
	}
	a.blocks[index].Thinking += thinking
	a.outputTokens += estimateKiroTokens(thinking)
	return a.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
	})
}

func (a *kiroAnthropicAssembler) addToolUse(id, name, partialJSON string) error {
	if err := a.closeActive(); err != nil {
		return err
	}
	if original := a.request.toolNameMap[name]; original != "" {
		name = original
	}
	var input map[string]any
	if strings.TrimSpace(partialJSON) == "" {
		input = map[string]any{}
	} else if err := json.Unmarshal([]byte(partialJSON), &input); err != nil {
		return fmt.Errorf("Kiro tool %s returned invalid JSON input: %w", name, err)
	}
	index := len(a.blocks)
	a.blocks = append(a.blocks, kiroResponseBlock{Type: "tool_use", ID: id, Name: name, Input: &input})
	a.hasToolUse = true
	a.outputTokens += estimateKiroTokens(partialJSON)
	if err := a.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	if err := a.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": partialJSON,
		},
	}); err != nil {
		return err
	}
	return a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (a *kiroAnthropicAssembler) ensureBlock(blockType string) (int, error) {
	if a.activeType == blockType && a.activeIndex >= 0 {
		return a.activeIndex, nil
	}
	if err := a.closeActive(); err != nil {
		return -1, err
	}
	index := len(a.blocks)
	block := kiroResponseBlock{Type: blockType}
	contentBlock := map[string]any{"type": blockType}
	if blockType == "text" {
		contentBlock["text"] = ""
	} else {
		contentBlock["thinking"] = ""
	}
	a.blocks = append(a.blocks, block)
	a.activeIndex = index
	a.activeType = blockType
	if err := a.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	}); err != nil {
		return -1, err
	}
	return index, nil
}

func (a *kiroAnthropicAssembler) closeActive() error {
	if a.activeIndex < 0 {
		return nil
	}
	index := a.activeIndex
	if a.activeType == "thinking" {
		signature := a.blocks[index].Signature
		if signature == "" {
			signature = "kiro"
			a.blocks[index].Signature = signature
		}
		if err := a.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{"type": "signature_delta", "signature": signature},
		}); err != nil {
			return err
		}
	}
	a.activeIndex = -1
	a.activeType = ""
	return a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (a *kiroAnthropicAssembler) finish() error {
	if a.finished {
		return nil
	}
	if err := a.flushInlineContent(); err != nil {
		return err
	}
	for id, tool := range a.tools {
		if strings.TrimSpace(tool.input.String()) != "" {
			return fmt.Errorf("Kiro tool %s (%s) ended before stop=true", tool.name, id)
		}
	}
	if err := a.closeActive(); err != nil {
		return err
	}
	if len(a.blocks) == 0 {
		if err := a.addText(" "); err != nil {
			return err
		}
		if err := a.closeActive(); err != nil {
			return err
		}
	}
	stopReason := "end_turn"
	if a.stopReason != "" {
		stopReason = a.stopReason
	} else if a.hasToolUse {
		stopReason = "tool_use"
	}
	usage := a.usage()
	if err := a.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	}); err != nil {
		return err
	}
	if err := a.emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	a.finished = true
	return nil
}

func (a *kiroAnthropicAssembler) response() map[string]any {
	stopReason := "end_turn"
	if a.stopReason != "" {
		stopReason = a.stopReason
	} else if a.hasToolUse {
		stopReason = "tool_use"
	}
	return map[string]any{
		"id":            a.messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         a.request.clientModel,
		"content":       a.blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         a.usage(),
	}
}

func (a *kiroAnthropicAssembler) usage() map[string]any {
	inputTokens := a.request.inputTokens
	if a.contextTokens > 0 {
		inputTokens = a.contextTokens
	}
	usage := map[string]any{
		"input_tokens":                inputTokens,
		"output_tokens":               a.outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
	if a.creditUnit != "" {
		usage["credit_usage"] = a.creditUsage
		usage["credit_unit"] = a.creditUnit
		usage["credit_unit_plural"] = a.creditPlural
	}
	return usage
}

func (a *kiroAnthropicAssembler) emit(event string, data any) error {
	if a.writer == nil {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.writer, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
	return nil
}

func processKiroEventStream(reader io.Reader, assembler *kiroAnthropicAssembler) error {
	for {
		frame, err := readKiroEventFrame(reader)
		if err == io.EOF {
			return assembler.finish()
		}
		if err != nil {
			return err
		}
		if err := assembler.process(frame); err != nil {
			return err
		}
	}
}

func kiroEventMessage(payload []byte) string {
	var value struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &value) == nil && value.Message != "" {
		return value.Message
	}
	return strings.TrimSpace(string(payload))
}

func kiroContextWindow(model string) int {
	switch model {
	case "claude-opus-5", "claude-sonnet-5",
		"claude-opus-4.8", "claude-opus-4.7",
		"claude-opus-4.6", "claude-sonnet-4.6":
		return 1_000_000
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return 272_000
	default:
		return 200_000
	}
}
