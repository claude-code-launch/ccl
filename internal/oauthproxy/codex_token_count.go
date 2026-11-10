package oauthproxy

import (
	"sync"

	"github.com/tiktoken-go/tokenizer/codec"
)

var (
	codexTokenizerOnce  sync.Once
	codexTokenizerCodec *codec.Codec
)

// countCodexResponsesInputTokens counts the request in the representation sent
// to the Codex Responses API.
func countCodexResponsesInputTokens(body []byte) (int, error) {
	codexTokenizerOnce.Do(func() {
		// CCL only counts o200k_base. The generic encoding dispatcher retains
		// every vocabulary in release binaries; use the concrete constructor so
		// the linker can discard the unused cl100k/p50k/r50k tables.
		codexTokenizerCodec = codec.NewO200kBase()
	})
	return codexTokenizerCodec.Count(string(body))
}

// conservativeCodexTokenEstimate is used only if the tokenizer fails.
func conservativeCodexTokenEstimate(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	return (len(raw) + 1) / 2
}
