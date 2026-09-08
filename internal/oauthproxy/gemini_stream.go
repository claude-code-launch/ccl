package oauthproxy

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// geminiStreamScannerBuffer bounds a single SSE line read from the Antigravity
// stream. Lines are small (one incremental part or a terminal usage/finish
// chunk), so 16 MiB is ample headroom.
const geminiStreamScannerBuffer = 16 << 20

// Signed parts must retain their original boundaries and all native fields.
const geminiPartSignaturePrefix = "ccl-gemini-part-v1:"

// geminiJSONPayload extracts the JSON payload from one Gemini SSE line,
// following the Antigravity stream format: trim, skip empty lines, `[DONE]` and
// `event:` lines, strip a `data:` prefix, and require a leading `{`.
func geminiJSONPayload(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "[DONE]" || strings.HasPrefix(trimmed, "event:") {
		return ""
	}
	if after, ok := strings.CutPrefix(trimmed, "data:"); ok {
		trimmed = strings.TrimSpace(after)
	}
	if trimmed == "" || trimmed[0] != '{' {
		return ""
	}
	return trimmed
}

// processGeminiStream consumes a Gemini/Antigravity SSE stream and emits an
// Anthropic Messages stream through the assembler. Each SSE chunk carries an
// incremental parts array (not a cumulative snapshot), so every part in every
// chunk is processed exactly once.
func processGeminiStream(reader io.Reader, assembler *anthropicResponseAssembler) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiStreamScannerBuffer)
	toolCounter := 0
	for scanner.Scan() {
		payload := geminiJSONPayload(scanner.Text())
		if payload == "" {
			continue
		}
		if err := processGeminiChunk([]byte(payload), assembler, &toolCounter); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Gemini stream: %w", err)
	}
	return assembler.finish()
}

// processGeminiNonStream consumes a non-streaming Gemini/Antigravity response
// body and emits a single Anthropic Messages response through the assembler.
func processGeminiNonStream(body []byte, assembler *anthropicResponseAssembler) error {
	toolCounter := 0
	if err := processGeminiChunk(body, assembler, &toolCounter); err != nil {
		return err
	}
	return assembler.finish()
}

// processGeminiChunk processes one Gemini response JSON object: it unwraps the
// Antigravity `response` envelope, feeds each part to the assembler, and applies
// the terminal usageMetadata/finishReason when the chunk carries them.
func processGeminiChunk(payload []byte, assembler *anthropicResponseAssembler, toolCounter *int) error {
	root := gjson.ParseBytes(payload)
	if !gjson.ValidBytes(payload) {
		return fmt.Errorf("invalid Gemini response JSON")
	}
	if upstreamError := root.Get("error"); upstreamError.Exists() {
		return fmt.Errorf("Gemini upstream error: %s", upstreamError.Raw)
	}
	responseNode := root.Get("response")
	if upstreamError := responseNode.Get("error"); upstreamError.Exists() {
		return fmt.Errorf("Gemini upstream error: %s", upstreamError.Raw)
	}
	if !responseNode.Exists() {
		if root.Get("candidates").Exists() {
			responseNode = root
		} else {
			return nil
		}
	}

	if parts := responseNode.Get("candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if part.Get("thoughtSignature").Exists() {
				if err := assembler.closeActive(); err != nil {
					return err
				}
				signature := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(part.Raw))
				if err := assembler.retain(len(signature)); err != nil {
					return err
				}
				index, err := assembler.ensureBlock("thinking")
				if err != nil {
					return err
				}
				assembler.blocks[index].Signature = signature
				if part.Get("thought").Bool() {
					if err := assembler.addThinking(part.Get("text").String()); err != nil {
						return err
					}
				}
				if err := assembler.closeActive(); err != nil {
					return err
				}
				if part.Get("thought").Bool() {
					continue
				}
			}
			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				name := strings.TrimSpace(functionCall.Get("name").String())
				if name == "" {
					continue
				}
				args := functionCall.Get("args").Raw
				if strings.TrimSpace(args) == "" || !gjson.Valid(args) {
					args = "{}"
				}
				(*toolCounter)++
				id := sanitizeClaudeToolID(name + "-" + strconv.Itoa(*toolCounter))
				if err := assembler.addToolUse(id, name, args); err != nil {
					return err
				}
				continue
			}
			text := part.Get("text")
			if !text.Exists() {
				continue
			}
			if part.Get("thought").Bool() {
				if err := assembler.addThinking(text.String()); err != nil {
					return err
				}
			} else if err := assembler.addText(text.String()); err != nil {
				return err
			}
			if part.Get("thoughtSignature").Exists() {
				if err := assembler.closeActive(); err != nil {
					return err
				}
			}
		}
	}

	// Terminal chunk: finishReason and usageMetadata arrive together.
	finishReason := strings.TrimSpace(responseNode.Get("candidates.0.finishReason").String())
	if finishReason == "" {
		return nil
	}
	usage := responseNode.Get("usageMetadata")
	if !usage.Exists() {
		usage = root.Get("usageMetadata")
	}
	if usage.Exists() {
		assembler.contextTokens = int(usage.Get("promptTokenCount").Int())
		assembler.outputTokens = int(usage.Get("candidatesTokenCount").Int() + usage.Get("thoughtsTokenCount").Int())
		assembler.cacheReadTokens = int(usage.Get("cachedContentTokenCount").Int())
	}
	if finishReason == "MAX_TOKENS" {
		assembler.stopReason = "max_tokens"
	}
	return nil
}
