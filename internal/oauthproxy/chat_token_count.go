package oauthproxy

import (
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer/codec"
)

var (
	chatCl100kOnce  sync.Once
	chatCl100kCodec *codec.Codec
)

// Count the converted request, including tool schemas. Only known OpenAI
// families select an OpenAI tokenizer; unrelated models use a conservative
// byte estimate instead of treating four Unicode characters as one token.
func countChatInputTokens(model string, body []byte) (int, string) {
	model = strings.ToLower(stripContextModelSuffix(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	var count int
	var err error
	estimator := "bytes_over_2_fallback"
	switch {
	case strings.HasPrefix(model, "gpt-5"), strings.HasPrefix(model, "gpt-4o"), strings.HasPrefix(model, "chatgpt-4o"), strings.HasPrefix(model, "gpt-4.1"), strings.HasPrefix(model, "gpt-4.5"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		count, err = countCodexResponsesInputTokens(body)
		estimator = "o200k_base"
	case strings.HasPrefix(model, "gpt-4"), strings.HasPrefix(model, "gpt-3.5"):
		chatCl100kOnce.Do(func() { chatCl100kCodec = codec.NewCl100kBase() })
		count, err = chatCl100kCodec.Count(string(body))
		estimator = "cl100k_base"
	default:
		return conservativeCodexTokenEstimate(body), estimator
	}
	if err != nil {
		return conservativeCodexTokenEstimate(body), "bytes_over_2_fallback"
	}
	return count, estimator
}
