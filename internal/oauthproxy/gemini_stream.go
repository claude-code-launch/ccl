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

type geminiStreamStepResult struct {
	rawParts           []string
	pendingSignedParts []string
	rawChunks          []string
	finishReason       string
}

func flushGeminiPendingSignedParts(assembler *responseAssembler, pending *[]string) error {
	if len(*pending) == 0 {
		return nil
	}
	items := *pending
	*pending = nil
	for _, rawPart := range items {
		if err := assembler.closeActive(); err != nil {
			return err
		}
		signature := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(rawPart))
		if err := assembler.retain(len(signature)); err != nil {
			return err
		}
		index, err := assembler.ensureBlock("thinking")
		if err != nil {
			return err
		}
		assembler.blocks[index].Signature = signature
		if err := assembler.closeActive(); err != nil {
			return err
		}
	}
	return nil
}

func assemblerHasVisibleOutput(assembler *responseAssembler) bool {
	if assembler.hasToolUse {
		return true
	}
	for i, block := range assembler.blocks {
		if block.Type == "text" && i < len(assembler.buffers) && assembler.buffers[i] != nil && strings.TrimSpace(assembler.buffers[i].text.String()) != "" {
			return true
		}
	}
	return false
}

func assemblerHasThinkingText(assembler *responseAssembler) bool {
	for _, buffer := range assembler.buffers {
		if buffer != nil && strings.TrimSpace(buffer.thinking.String()) != "" {
			return true
		}
	}
	return false
}

func processGeminiStreamStep(reader io.Reader, assembler *responseAssembler, toolCounter *int) (*geminiStreamStepResult, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiStreamScannerBuffer)
	result := &geminiStreamStepResult{}
	terminalSeen := false
	for scanner.Scan() {
		payload := geminiJSONPayload(scanner.Text())
		if payload == "" {
			continue
		}
		if len(result.rawChunks) < 8 && len(payload) <= 2048 {
			result.rawChunks = append(result.rawChunks, payload)
		}
		if err := processGeminiChunkWithResult([]byte(payload), assembler, toolCounter, result); err != nil {
			return result, err
		}
		root := gjson.Parse(payload)
		if fr := strings.TrimSpace(geminiResponsePayload(root).Get("candidates.0.finishReason").String()); fr != "" {
			result.finishReason = fr
			terminalSeen = true
		}
		if geminiPromptBlockReason(root) != "" {
			terminalSeen = true
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read Gemini stream: %w", err)
	}
	if !terminalSeen {
		return result, fmt.Errorf("Gemini stream ended before finishReason: %w", io.ErrUnexpectedEOF)
	}
	return result, nil
}

// processGeminiStream consumes a Gemini/Antigravity SSE stream and emits an
// OpenAI Responses stream through the assembler. Each SSE chunk carries an
// incremental parts array (not a cumulative snapshot), so every part in every
// chunk is processed exactly once.
func processGeminiStream(reader io.Reader, assembler *responseAssembler) error {
	toolCounter := 0
	result, err := processGeminiStreamStep(reader, assembler, &toolCounter)
	if err != nil {
		return err
	}
	if err := flushGeminiPendingSignedParts(assembler, &result.pendingSignedParts); err != nil {
		return err
	}
	if err := ensureGeminiVisibleOutput(assembler); err != nil {
		return err
	}
	return assembler.finish()
}

// processGeminiNonStream consumes a non-streaming Gemini/Antigravity response
// body and emits a single OpenAI Responses response through the assembler.
func processGeminiNonStream(body []byte, assembler *responseAssembler) error {
	toolCounter := 0
	result := &geminiStreamStepResult{}
	if err := processGeminiChunkWithResult(body, assembler, &toolCounter, result); err != nil {
		return err
	}
	root := gjson.ParseBytes(body)
	if geminiResponsePayload(root).Get("candidates.0.finishReason").String() == "" && geminiPromptBlockReason(root) == "" {
		return fmt.Errorf("Gemini response ended without a finishReason or prompt block reason: %w", io.ErrUnexpectedEOF)
	}
	if err := flushGeminiPendingSignedParts(assembler, &result.pendingSignedParts); err != nil {
		return err
	}
	if err := ensureGeminiVisibleOutput(assembler); err != nil {
		return err
	}
	return assembler.finish()
}

// ensureGeminiVisibleOutput promotes accumulated thinking text to a visible text
// block when a Gemini thinking model finishes a turn (STOP or MAX_TOKENS)
// having emitted only thought parts and no text or tool_use blocks. Without
// this fallback, Claude Code collapses the thinking block into "Baked for Xs · done"
// with no visible response.
func ensureGeminiVisibleOutput(assembler *responseAssembler) error {
	if assembler.hasToolUse {
		return nil
	}
	for _, block := range assembler.blocks {
		if block.Type == "text" {
			return nil
		}
	}
	var thinking strings.Builder
	for _, buffer := range assembler.buffers {
		if buffer != nil && buffer.thinking.Len() > 0 {
			thinking.WriteString(buffer.thinking.String())
		}
	}
	text := strings.TrimSpace(thinking.String())
	if text == "" {
		return nil
	}
	if err := assembler.closeActive(); err != nil {
		return err
	}
	model := ""
	if assembler.request != nil {
		model = assembler.request.upstreamModel
	}
	LogWarnEvent("gemini_thinking_only_response", "component", "gemini",
		"model", model, "stop_reason", assembler.stopReason, "thinking_bytes", len(text))
	return assembler.addText(text)
}

func geminiResponsePayload(root gjson.Result) gjson.Result {
	if response := root.Get("response"); response.Exists() {
		return response
	}
	return root
}

func geminiPromptBlockReason(root gjson.Result) string {
	reason := geminiResponsePayload(root).Get("promptFeedback.blockReason").String()
	if reason == "" {
		reason = root.Get("promptFeedback.blockReason").String()
	}
	if reason == "BLOCK_REASON_UNSPECIFIED" {
		return ""
	}
	return reason
}

func geminiRefusalReason(reason string) bool {
	switch reason {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return true
	}
	return false
}

// processGeminiChunk processes one Gemini response JSON object: it unwraps the
// Antigravity `response` envelope, feeds each part to the assembler, and applies
// the terminal usageMetadata/finishReason when the chunk carries them.
func processGeminiChunk(payload []byte, assembler *responseAssembler, toolCounter *int) error {
	result := &geminiStreamStepResult{}
	if err := processGeminiChunkWithResult(payload, assembler, toolCounter, result); err != nil {
		return err
	}
	return flushGeminiPendingSignedParts(assembler, &result.pendingSignedParts)
}

func processGeminiChunkWithResult(payload []byte, assembler *responseAssembler, toolCounter *int, result *geminiStreamStepResult) error {
	root := gjson.ParseBytes(payload)
	if !gjson.ValidBytes(payload) {
		return fmt.Errorf("invalid Gemini response JSON")
	}
	if upstreamError := root.Get("error"); upstreamError.Exists() {
		return fmt.Errorf("Gemini upstream error: %s", upstreamError.Raw)
	}
	responseNode := geminiResponsePayload(root)
	if upstreamError := responseNode.Get("error"); upstreamError.Exists() {
		return fmt.Errorf("Gemini upstream error: %s", upstreamError.Raw)
	}
	finishReason := strings.TrimSpace(responseNode.Get("candidates.0.finishReason").String())
	blockReason := geminiPromptBlockReason(root)
	if blockReason != "" || geminiRefusalReason(finishReason) {
		if blockReason == "" {
			blockReason = finishReason
		}
		assembler.stopReason = "refusal"
		if err := assembler.addText("Gemini blocked this response (" + blockReason + ")."); err != nil {
			return err
		}
		geminiApplyUsage(root, assembler)
		return nil
	}
	if finishReason != "" && finishReason != "STOP" && finishReason != "MAX_TOKENS" {
		return fmt.Errorf("Gemini generation failed: %s", finishReason)
	}

	if parts := responseNode.Get("candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if result != nil {
				result.rawParts = append(result.rawParts, part.Raw)
			}
			if part.Get("thoughtSignature").Exists() && !part.Get("functionCall").Exists() && part.Get("text").String() == "" && assembler.activeType != "thinking" && result != nil {
				result.pendingSignedParts = append(result.pendingSignedParts, part.Raw)
				continue
			}
			if result != nil && len(result.pendingSignedParts) > 0 {
				if err := flushGeminiPendingSignedParts(assembler, &result.pendingSignedParts); err != nil {
					return err
				}
			}
			if part.Get("thoughtSignature").Exists() {
				if part.Get("thought").Bool() || (assembler.activeType == "thinking" && !part.Get("functionCall").Exists() && part.Get("text").String() == "") {
					index, err := assembler.ensureBlock("thinking")
					if err != nil {
						return err
					}
					if err := assembler.addThinking(part.Get("text").String()); err != nil {
						return err
					}
					signature := geminiPartSignaturePrefix + base64.StdEncoding.EncodeToString([]byte(part.Raw))
					if err := assembler.retain(len(signature)); err != nil {
						return err
					}
					assembler.blocks[index].Signature = signature
					if err := assembler.closeActive(); err != nil {
						return err
					}
					continue
				}
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
				if err := assembler.closeActive(); err != nil {
					return err
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
				id := functionCall.Get("id").String()
				if id == "" {
					// Gemini may omit IDs. Generate one only in that case; native
					// IDs must survive so subsequent tool results stay correlated.
					id = sanitizeClaudeToolID(name + "-" + strings.ReplaceAll(uuidString(), "-", "") + "_" + strconv.Itoa(*toolCounter))
				}
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

	geminiApplyUsage(root, assembler)
	if finishReason == "MAX_TOKENS" {
		assembler.stopReason = "max_tokens"
	}
	return nil
}

func geminiApplyUsage(root gjson.Result, assembler *responseAssembler) {
	usage := geminiResponsePayload(root).Get("usageMetadata")
	if !usage.Exists() {
		usage = root.Get("usageMetadata")
	}
	if usage.Exists() {
		if cached := usage.Get("cachedContentTokenCount"); cached.Exists() {
			assembler.cacheReadTokens = int(cached.Int())
		}
		// Gemini promptTokenCount already includes cached content tokens; subtract
		// them so input_tokens holds only fresh input (see codex_responses_stream).
		if in := usage.Get("promptTokenCount"); in.Exists() {
			assembler.contextTokens = max(0, int(in.Int())-assembler.cacheReadTokens)
			assembler.inputUsageKnown = true
		}
		if usage.Get("candidatesTokenCount").Exists() || usage.Get("thoughtsTokenCount").Exists() {
			assembler.outputTokens = int(usage.Get("candidatesTokenCount").Int() + usage.Get("thoughtsTokenCount").Int())
		}
	}
}
