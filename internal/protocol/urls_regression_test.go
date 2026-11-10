package protocol

import "testing"

func TestEndpointNormalizationKeepsQueryAndEscaping(t *testing.T) {
	for _, source := range []string{"/models", "/responses", "/chat/completions", ""} {
		for target, normalize := range map[string]func(string) string{"/models": NormalizeOpenAIModelsURL, "/responses": NormalizeOpenAIResponsesURL, "/chat/completions": NormalizeOpenAIChatCompletionsURL} {
			base := "https://example.com/a%2Fb/v1"
			query := "?api-version=1&next=%2F#fragment"
			if got := normalize(base + source + "/" + query); got != base+target+query {
				t.Fatalf("got %s want %s", got, base+target+query)
			}
		}
	}
}
