package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// ProbeOpenAIResponsesStatusContext is ProbeOpenAIResponsesSupportContext but
// reports the upstream HTTP status code (0 on transport error) instead of a
// boolean. Callers that need to distinguish an auth rejection (401/403) from a
// valid key with a model/parameter/rate-limit problem use this variant.
func ProbeOpenAIResponsesStatusContext(parent context.Context, endpoint, apiKey, model string, timeout time.Duration) (int, error) {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "hi"}}},
		},
		"max_output_tokens": 1,
		"store":             false,
	})
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeOpenAIResponsesURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
