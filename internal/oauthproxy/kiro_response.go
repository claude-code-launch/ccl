package oauthproxy

import (
	"bufio"
	"bytes"
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

// kiroBlockBuffer accumulates the streamed text/thinking of one content block.
// Appending to a strings.Builder keeps assembly linear; concatenating onto the
// block's string field would copy the whole block on every delta.
type kiroBlockBuffer struct {
	text     strings.Builder
	thinking strings.Builder
}

// Server-sent event payloads on the streaming hot path. These are typed structs
// rather than nested maps so each delta costs one small allocation instead of
// three maps plus reflection over them.
type kiroBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type kiroBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type kiroBlockIndexEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type kiroTextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type kiroThinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type kiroSignatureDelta struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

type kiroInputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type kiroTextBlockStart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type kiroThinkingBlockStart struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type kiroRedactedThinkingBlockStart struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type kiroToolUseBlockStart struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type kiroAnthropicAssembler struct {
	request         *kiroConvertedRequest
	writer          http.ResponseWriter
	flusher         http.Flusher
	messageID       string
	blocks          []kiroResponseBlock
	buffers         []*kiroBlockBuffer
	eventBuffer     bytes.Buffer
	encoder         *json.Encoder
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
	assembler.encoder = json.NewEncoder(&assembler.eventBuffer)
	return assembler
}

// appendBlock registers a new content block and its accumulation buffer, keeping
// both slices index-aligned.
func (a *kiroAnthropicAssembler) appendBlock(block kiroResponseBlock) int {
	index := len(a.blocks)
	a.blocks = append(a.blocks, block)
	a.buffers = append(a.buffers, &kiroBlockBuffer{})
	return index
}

// contentBlocks materializes the accumulated text/thinking into a copy of the
// block list. Call it only when a full response body is needed.
func (a *kiroAnthropicAssembler) contentBlocks() []kiroResponseBlock {
	blocks := make([]kiroResponseBlock, len(a.blocks))
	copy(blocks, a.blocks)
	for index := range blocks {
		if index >= len(a.buffers) || a.buffers[index] == nil {
			continue
		}
		if buffer := a.buffers[index]; buffer.text.Len() > 0 {
			blocks[index].Text = buffer.text.String()
		}
		if buffer := a.buffers[index]; buffer.thinking.Len() > 0 {
			blocks[index].Thinking = buffer.thinking.String()
		}
	}
	return blocks
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
			index := a.appendBlock(kiroResponseBlock{Type: "redacted_thinking", Data: event.RedactedContent})
			if err := a.emit("content_block_start", kiroBlockStartEvent{
				Type:  "content_block_start",
				Index: index,
				ContentBlock: kiroRedactedThinkingBlockStart{
					Type: "redacted_thinking",
					Data: event.RedactedContent,
				},
			}); err != nil {
				return err
			}
			if err := a.emit("content_block_stop", kiroBlockIndexEvent{Type: "content_block_stop", Index: index}); err != nil {
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
	a.buffers[index].text.WriteString(text)
	a.outputTokens += estimateKiroTokens(text)
	return a.emit("content_block_delta", kiroBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: kiroTextDelta{Type: "text_delta", Text: text},
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
	a.buffers[index].thinking.WriteString(thinking)
	a.outputTokens += estimateKiroTokens(thinking)
	return a.emit("content_block_delta", kiroBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: kiroThinkingDelta{Type: "thinking_delta", Thinking: thinking},
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
	index := a.appendBlock(kiroResponseBlock{Type: "tool_use", ID: id, Name: name, Input: &input})
	a.hasToolUse = true
	a.outputTokens += estimateKiroTokens(partialJSON)
	if err := a.emit("content_block_start", kiroBlockStartEvent{
		Type:  "content_block_start",
		Index: index,
		ContentBlock: kiroToolUseBlockStart{
			Type:  "tool_use",
			ID:    id,
			Name:  name,
			Input: map[string]any{},
		},
	}); err != nil {
		return err
	}
	if err := a.emit("content_block_delta", kiroBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: kiroInputJSONDelta{Type: "input_json_delta", PartialJSON: partialJSON},
	}); err != nil {
		return err
	}
	return a.emit("content_block_stop", kiroBlockIndexEvent{Type: "content_block_stop", Index: index})
}

func (a *kiroAnthropicAssembler) ensureBlock(blockType string) (int, error) {
	if a.activeType == blockType && a.activeIndex >= 0 {
		return a.activeIndex, nil
	}
	if err := a.closeActive(); err != nil {
		return -1, err
	}
	var contentBlock any = kiroThinkingBlockStart{Type: blockType}
	if blockType == "text" {
		contentBlock = kiroTextBlockStart{Type: blockType}
	}
	index := a.appendBlock(kiroResponseBlock{Type: blockType})
	a.activeIndex = index
	a.activeType = blockType
	if err := a.emit("content_block_start", kiroBlockStartEvent{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: contentBlock,
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
		if err := a.emit("content_block_delta", kiroBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: index,
			Delta: kiroSignatureDelta{Type: "signature_delta", Signature: signature},
		}); err != nil {
			return err
		}
	}
	a.activeIndex = -1
	a.activeType = ""
	return a.emit("content_block_stop", kiroBlockIndexEvent{Type: "content_block_stop", Index: index})
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
	usage := a.usage()
	if err := a.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   a.resolvedStopReason(),
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

// resolvedStopReason reports the Anthropic stop reason for the assembled turn.
func (a *kiroAnthropicAssembler) resolvedStopReason() string {
	if a.stopReason != "" {
		return a.stopReason
	}
	if a.hasToolUse {
		return "tool_use"
	}
	return "end_turn"
}

func (a *kiroAnthropicAssembler) response() map[string]any {
	return map[string]any{
		"id":            a.messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         a.request.clientModel,
		"content":       a.contentBlocks(),
		"stop_reason":   a.resolvedStopReason(),
		"stop_sequence": nil,
		"usage":         a.usage(),
	}
}

// tokenTotals returns the input/output token counts for this turn, using the
// same fields the Anthropic usage object reports so the session summary matches
// what was actually billed against the account.
func (a *kiroAnthropicAssembler) tokenTotals() (input, output int) {
	input = a.request.inputTokens
	if a.contextTokens > 0 {
		input = a.contextTokens
	}
	return input, a.outputTokens
}

func (a *kiroAnthropicAssembler) usage() map[string]any {
	inputTokens, outputTokens := a.tokenTotals()
	usage := map[string]any{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
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
	// Serialize straight into a reused buffer: one write per event, and no
	// intermediate []byte/format allocation per streamed token.
	a.eventBuffer.Reset()
	a.eventBuffer.WriteString("event: ")
	a.eventBuffer.WriteString(event)
	a.eventBuffer.WriteString("\ndata: ")
	if err := a.encoder.Encode(data); err != nil {
		return err
	}
	// Encode already appended the record newline; SSE needs a blank line too.
	a.eventBuffer.WriteByte('\n')
	if _, err := a.writer.Write(a.eventBuffer.Bytes()); err != nil {
		return err
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
	return nil
}

func processKiroEventStream(reader io.Reader, assembler *kiroAnthropicAssembler) error {
	// Frames are read in two small chunks (prelude, then remainder), so reading
	// straight from the network body would cost two syscalls per frame.
	buffered := bufio.NewReaderSize(reader, kiroEventReadBufferSize)
	for {
		frame, err := readKiroEventFrame(buffered)
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
