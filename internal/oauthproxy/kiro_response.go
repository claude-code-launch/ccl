package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func (a *anthropicResponseAssembler) process(frame *kiroEventFrame) error {
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

func (a *anthropicResponseAssembler) processEvent(eventType string, payload []byte) error {
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
			if err := a.retain(len(event.RedactedContent)); err != nil {
				return err
			}
			if len(a.blocks) >= anthropicAssemblerMaxBlocks {
				return fmt.Errorf("Kiro response exceeds %d content blocks", anthropicAssemblerMaxBlocks)
			}
			if err := a.closeActive(); err != nil {
				return err
			}
			index := a.appendBlock(anthropicResponseBlock{Type: "redacted_thinking", Data: event.RedactedContent})
			if err := a.emit("content_block_start", anthropicBlockStartEvent{
				Type:  "content_block_start",
				Index: index,
				ContentBlock: anthropicRedactedThinkingBlockStart{
					Type: "redacted_thinking",
					Data: event.RedactedContent,
				},
			}); err != nil {
				return err
			}
			if err := a.emit("content_block_stop", anthropicBlockIndexEvent{Type: "content_block_stop", Index: index}); err != nil {
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
			if len(a.tools) >= anthropicAssemblerMaxToolCalls {
				return fmt.Errorf("Kiro tool use stream exceeds %d concurrent tool calls", anthropicAssemblerMaxToolCalls)
			}
			accumulator = &anthropicToolAccumulator{name: event.Name}
			a.tools[event.ToolUseID] = accumulator
		}
		if event.Name != "" {
			accumulator.name = event.Name
		}
		if err := a.retain(len(event.Input)); err != nil {
			return err
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
			a.contextTokens = int(event.ContextUsagePercentage / 100 * float64(kiroContextWindow(a.request.upstreamModel)))
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

func processKiroEventStream(reader io.Reader, assembler *anthropicResponseAssembler) error {
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
