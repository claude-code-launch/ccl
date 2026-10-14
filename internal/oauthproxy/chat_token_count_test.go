package oauthproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiktoken-go/tokenizer/codec"
)

func TestChatCountTokensUsesRoutedModelAndConvertedPayload(t *testing.T) {
	// Unicode escapes keep this test compatible with the internal language rule.
	text := strings.Repeat("\u8bf7\u68c0\u67e5\u4ee3\u7801\u4e2d\u7684\u5e76\u53d1\u95ee\u9898", 200)
	payload, _ := json.Marshal(map[string]any{"model": "sonnet", "messages": []map[string]any{{"role": "user", "content": text}}, "tools": []map[string]any{{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}}}}})
	for _, tc := range []struct {
		model   string
		encoder *codec.Codec
	}{
		{"gpt-5.6-sol", codec.NewO200kBase()},
		{"gpt-4-turbo", codec.NewCl100kBase()},
	} {
		t.Run(tc.model, func(t *testing.T) {
			s := newChatCompletionsService("fake-session", "http://unused.invalid", []runtimeModelRoute{{Alias: "sonnet", Name: tc.model}}, "fake-key", nil)
			converted, err := s.convertRequest(payload)
			if err != nil {
				t.Fatal(err)
			}
			want, err := tc.encoder.Count(string(converted.body))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(payload))
			r.Header.Set("x-api-key", "fake-session")
			w := httptest.NewRecorder()
			s.handleCountTokens(w, r)
			var got struct {
				Input int `json:"input_tokens"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || got.Input != want {
				t.Fatalf("status=%d count=%d, want %d", w.Code, got.Input, want)
			}
		})
	}
}

func TestUnknownTokenizerUsesConservativeUnicodeEstimate(t *testing.T) {
	body := []byte(strings.Repeat("\u5e76\u53d1\u8c03\u7528\u4ee3\u7801", 100))
	known, _ := countCodexResponsesInputTokens(body)
	unknown, estimator := countChatInputTokens("unrelated-provider-model", body)
	if estimator != "bytes_over_2_fallback" || unknown < known {
		t.Fatalf("unknown estimate=%d known=%d estimator=%s", unknown, known, estimator)
	}
	if got := estimateApproxTokensBytes(body); got < known {
		t.Fatalf("generic fallback still undercounts Unicode: %d < %d", got, known)
	}
}
