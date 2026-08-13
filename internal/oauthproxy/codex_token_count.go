package oauthproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

const (
	// The Codex subscription backend has a 1M-class window. Claude Code's
	// displayed usage can come from a previous provider with a different
	// tokenizer, so leave room for that mismatch and the generated summary.
	// A compact request may be routed to an ordinary 200K model. Keep enough
	// room for the generated summary and tokenizer differences instead of
	// filling the model window with input alone. The user-visible result still
	// summarizes the newest retained history; older items are discarded locally.
	codexCompactionSoftTargetTokens  = 120_000
	codexCompactionRetryTargetTokens = 80_000
	codexCompactionTruncationNotice  = "[earlier conversation truncated by ccl to recover an oversized compaction request]"
)

var (
	codexTokenizerOnce  sync.Once
	codexTokenizerCodec tokenizer.Codec
	codexTokenizerErr   error
)

// countCodexResponsesInputTokens counts the request in the representation sent
// to the Codex Responses API. Counting the inbound Anthropic JSON with a fixed
// characters-per-token ratio substantially underestimates code, tool schemas,
// and escaped JSON, which can postpone Claude Code compaction until the
// upstream context window has already been exceeded.
func countCodexResponsesInputTokens(body []byte) (int, error) {
	codexTokenizerOnce.Do(func() {
		codexTokenizerCodec, codexTokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	if codexTokenizerErr != nil {
		return 0, codexTokenizerErr
	}
	return codexTokenizerCodec.Count(string(body))
}

// conservativeCodexTokenEstimate is used only if the tokenizer fails. One
// token per two bytes intentionally errs toward early compaction instead of
// allowing another oversized request through.
func conservativeCodexTokenEstimate(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	return (len(raw) + 1) / 2
}

// codexCompactionRecoveryTarget returns a target that is both below the fixed
// retry ceiling and materially smaller than the body that just overflowed.
// Message-boundary trimming can land well below the preflight target, so using
// only the fixed ceiling could otherwise produce no change and skip recovery.
func codexCompactionRecoveryTarget(body []byte) int {
	target := codexCompactionRetryTargetTokens
	tokens, err := countCodexResponsesInputTokens(body)
	if err != nil {
		return target
	}
	if reduced := tokens * 2 / 3; reduced > 0 && reduced < target {
		target = reduced
	}
	return target
}

type codexCompactionTrimStats struct {
	originalTokens  int
	finalTokens     int
	droppedItems    int
	tokenizerPasses int
}

// trimCodexCompactionBody removes only an oldest prefix of conversation items,
// preserving the summarizer system message and the newest conversation. It is
// deliberately restricted to requests positively identified as Claude Code
// compaction requests; normal turns are never silently shortened.
func trimCodexCompactionBody(body []byte, targetTokens int) ([]byte, codexCompactionTrimStats, error) {
	stats := codexCompactionTrimStats{}
	if targetTokens <= 0 {
		return nil, stats, fmt.Errorf("compaction token target must be positive")
	}
	originalTokens, err := countCodexResponsesInputTokens(body)
	if err != nil {
		return nil, stats, err
	}
	stats.originalTokens = originalTokens
	stats.finalTokens = originalTokens
	stats.tokenizerPasses = 1
	if originalTokens <= targetTokens {
		return body, stats, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, stats, fmt.Errorf("decode Codex compaction request: %w", err)
	}
	input, _ := payload["input"].([]any)
	if len(input) < 2 {
		return nil, stats, fmt.Errorf("Codex compaction request has no removable history")
	}
	protected := 0
	if first, _ := input[0].(map[string]any); strings.EqualFold(stringValue(first["role"]), "developer") {
		protected = 1
	}
	if len(input)-protected < 2 {
		return nil, stats, fmt.Errorf("Codex compaction request has no removable history")
	}

	build := func(start int) ([]byte, int, error) {
		kept := make([]any, 0, protected+1+len(input)-start)
		kept = append(kept, input[:protected]...)
		kept = append(kept, map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": codexCompactionTruncationNotice}},
		})
		kept = append(kept, codexDropOrphanedToolOutputs(input[start:])...)
		candidate := make(map[string]any, len(payload))
		for key, value := range payload {
			candidate[key] = value
		}
		candidate["input"] = kept
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return nil, 0, err
		}
		tokens, err := countCodexResponsesInputTokens(encoded)
		return encoded, tokens, err
	}

	// Estimate the initial cut from the whole body's measured token density. The
	// previous formula multiplied the removable fraction by a 15% safety factor;
	// once a request was around 90% over target that exceeded 100% of removable
	// history and retained only the compact prompt. Starting at the global byte
	// estimate preserves a recent suffix near the target, while exact tokenizer
	// passes below can still remove more when newer code/tool output is denser.
	sizes := make([]int, len(input))
	removableBytes := 0
	for index := protected; index < len(input)-1; index++ {
		encoded, err := json.Marshal(input[index])
		if err != nil {
			return nil, stats, fmt.Errorf("measure Codex compaction item: %w", err)
		}
		sizes[index] = len(encoded) + 1
		removableBytes += sizes[index]
	}
	needTokens := originalTokens - targetTokens
	needBytes := (len(body)*needTokens + originalTokens - 1) / originalTokens
	if needBytes > removableBytes {
		needBytes = removableBytes
	}
	start, removedBytes := protected, 0
	for start < len(input)-1 && removedBytes < needBytes {
		removedBytes += sizes[start]
		start++
	}
	if start == protected {
		start++
	}

	// Token density can vary between prose, source code, and tool results. Verify
	// the estimated cut and make at most two proportional corrections. The final
	// fallback keeps only the newest item, bounding work at five tokenizer passes
	// including the initial count.
	lastTriedStart := -1
	for attempt := 0; attempt < 3 && start < len(input); attempt++ {
		lastTriedStart = start
		candidate, tokens, err := build(start)
		stats.tokenizerPasses++
		if err != nil {
			return nil, stats, fmt.Errorf("trim Codex compaction request: %w", err)
		}
		if tokens <= targetTokens {
			stats.finalTokens = tokens
			stats.droppedItems = start - protected
			return candidate, stats, nil
		}
		if start >= len(input)-1 {
			break
		}
		remainingBytes := 0
		for index := start; index < len(input)-1; index++ {
			remainingBytes += sizes[index]
		}
		excess := tokens - targetTokens
		additionalBytes := (remainingBytes*excess*125 + tokens*100 - 1) / (tokens * 100)
		if minimum := (remainingBytes + 15) / 16; additionalBytes < minimum {
			additionalBytes = minimum
		}
		removedBytes = 0
		for start < len(input)-1 && removedBytes < additionalBytes {
			removedBytes += sizes[start]
			start++
		}
	}
	if lastTriedStart != len(input)-1 {
		candidate, tokens, err := build(len(input) - 1)
		stats.tokenizerPasses++
		if err != nil {
			return nil, stats, fmt.Errorf("trim Codex compaction request: %w", err)
		}
		if tokens <= targetTokens {
			stats.finalTokens = tokens
			stats.droppedItems = len(input) - 1 - protected
			return candidate, stats, nil
		}
	}
	return nil, stats, fmt.Errorf("newest Codex compaction item alone exceeds %d tokens", targetTokens)
}

func codexDropOrphanedToolOutputs(input []any) []any {
	calls := make(map[string]bool)
	for _, item := range input {
		value, _ := item.(map[string]any)
		if stringValue(value["type"]) == "function_call" {
			calls[stringValue(value["call_id"])] = true
		}
	}
	kept := make([]any, 0, len(input))
	for _, item := range input {
		value, _ := item.(map[string]any)
		if stringValue(value["type"]) == "function_call_output" && !calls[stringValue(value["call_id"])] {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func isCodexContextOverflow(err error) bool {
	var upstreamErr *codexResponsesUpstreamError
	message := ""
	if errors.As(err, &upstreamErr) {
		if upstreamErr.status != http.StatusBadRequest {
			return false
		}
		message = upstreamErr.body
	} else if err != nil {
		// Codex may accept the HTTP request with 200 and report the context
		// overflow later as an SSE error event.
		message = err.Error()
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "context window") ||
		strings.Contains(message, "context length") ||
		strings.Contains(message, "input exceeds") ||
		strings.Contains(message, "too many tokens")
}
