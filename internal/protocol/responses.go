package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// ProbeOpenAIResponsesSupport sends a minimal, real generation request to the
// /v1/responses endpoint to determine whether an OpenAI-compatible gateway
// implements the newer Responses API ("openai(responses)") as
// opposed to only the legacy Chat Completions API ("openai(chat)"). Listing
// models alone (/v1/models) cannot distinguish these two, since both protocols
// commonly share the same model catalog — an actual call to /v1/responses is
// required. Returns true only when the upstream responds with a 2xx status.
func ProbeOpenAIResponsesSupport(endpoint, apiKey, model string, timeout time.Duration) bool {
	return ProbeOpenAIResponsesSupportContext(context.Background(), endpoint, apiKey, model, timeout)
}

// ProbeOpenAIResponsesSupportContext is ProbeOpenAIResponsesSupport with caller-controlled cancellation.
func ProbeOpenAIResponsesSupportContext(parent context.Context, endpoint, apiKey, model string, timeout time.Duration) bool {
	status, err := ProbeOpenAIResponsesStatusContext(parent, endpoint, apiKey, model, timeout)
	if err != nil {
		return false
	}
	return status >= 200 && status < 300
}

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
