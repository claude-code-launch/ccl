package oauthproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// commandcodeStreamScannerBuffer bounds a single NDJSON line from the
// /alpha/generate stream. Text/tool deltas are small, so 16 MiB is ample.
const commandcodeStreamScannerBuffer = 16 << 20

// processCommandCodeStream consumes a CommandCode NDJSON stream and emits an
// Anthropic Messages SSE stream through the assembler. The caller must have
// already called assembler.start(); finish() closes the stream.
func processCommandCodeStream(reader io.Reader, assembler *anthropicResponseAssembler) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), commandcodeStreamScannerBuffer)
	if err := processCommandCodeEvents(scanner, assembler); err != nil {
		return err
	}
	return assembler.finish()
}

// processCommandCodeNonStream consumes a fully-read NDJSON body and returns the
// accumulated Anthropic message; the assembler's writer stays nil so events are
// buffered into content blocks rather than streamed.
func processCommandCodeNonStream(body []byte, assembler *anthropicResponseAssembler) error {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), commandcodeStreamScannerBuffer)
	if err := processCommandCodeEvents(scanner, assembler); err != nil {
		return err
	}
	// finish() finalizes the message (stop_reason, usage) exactly like the
	// streaming path; with a nil writer it only closes the assembled content.
	return assembler.finish()
}

// processCommandCodeEvents dispatches NDJSON lines. Event shapes mirror the
// reference client: text-delta/reasoning-delta carry incremental text, tool-call
// carries a complete call, finish-step/finish carry usage and finishReason, and
// error/start/lifecycle events are ignorable.
func processCommandCodeEvents(scanner *bufio.Scanner, assembler *anthropicResponseAssembler) error {
	pendingFinish := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "[DONE]" || strings.HasPrefix(line, ":") {
			continue
		}
		root := gjson.Parse(line)
		switch root.Get("type").String() {
		case "text-delta":
			text := root.Get("text").String()
			if text == "" {
				text = root.Get("delta").String()
			}
			if text != "" {
				if err := assembler.addText(text); err != nil {
					return err
				}
			}
		case "reasoning-delta":
			if text := root.Get("text").String(); text != "" {
				if err := assembler.addThinking(text); err != nil {
					return err
				}
			}
		case "tool-call":
			id := strings.TrimSpace(root.Get("toolCallId").String())
			name := strings.TrimSpace(root.Get("toolName").String())
			if name == "" {
				continue
			}
			if err := assembler.addToolUse(id, name, commandcodeToolInputJSON(root.Get("input"))); err != nil {
				return err
			}
		case "finish-step":
			if reason := strings.TrimSpace(root.Get("finishReason").String()); reason != "" {
				pendingFinish = reason
			}
			commandcodeApplyUsage(assembler, root.Get("usage"))
		case "finish":
			if reason := strings.TrimSpace(root.Get("finishReason").String()); reason != "" {
				pendingFinish = reason
			}
			// totalUsage is authoritative; fall back to the last finish-step usage.
			usage := root.Get("totalUsage")
			if !usage.Exists() {
				usage = root.Get("usage")
			}
			commandcodeApplyUsage(assembler, usage)
			if stop := commandcodeStopReason(pendingFinish); stop != "" {
				assembler.stopReason = stop
			}
		case "error":
			// A mid-stream error event is advisory; the stream's terminal finish
			// (or HTTP status) is what drives error handling upstream.
			LogErrorEvent("commandcode_stream_error", "message",
				root.Get("error.message").String()+root.Get("message").String())
		default:
			// start, start-step, text-start/end, reasoning-start/end,
			// tool-input-start/delta/end, tool-error, provider-metadata.
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read CommandCode stream: %w", err)
	}
	return nil
}

// commandcodeApplyUsage copies CommandCode usage onto the assembler.
func commandcodeApplyUsage(assembler *anthropicResponseAssembler, usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	assembler.contextTokens = int(usage.Get("inputTokens").Int())
	assembler.outputTokens = int(usage.Get("outputTokens").Int())
	assembler.cacheReadTokens = int(usage.Get("cachedInputTokens").Int())
}

// commandcodeStopReason maps a CommandCode finish reason onto the Anthropic
// stop_reason the assembler should report. Only length needs an explicit
// override: tool-calls and stop are already handled by resolvedStopReason via
// hasToolUse / the end_turn default.
func commandcodeStopReason(reason string) string {
	if reason == "length" {
		return "max_tokens"
	}
	return ""
}

// commandcodeToolInputJSON normalizes a tool-call input the way the reference
// client does: a JSON-string input is passed through, an object is serialized,
// and a missing value becomes an empty object.
func commandcodeToolInputJSON(input gjson.Result) string {
	if input.Type == gjson.String {
		return input.String()
	}
	if raw := input.Raw; raw != "" && gjson.Valid(raw) {
		return raw
	}
	return "{}"
}
