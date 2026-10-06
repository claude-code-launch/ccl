package oauthproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestZedFoldErrorMapsErrorTypesToStatuses pins the status each Anthropic error
// type becomes when a stream-only upstream serves a non-streaming client.
func TestZedFoldErrorMapsErrorTypesToStatuses(t *testing.T) {
	cases := map[string]int{
		"overloaded_error":      529,
		"rate_limit_error":      http.StatusTooManyRequests,
		"authentication_error":  http.StatusUnauthorized,
		"permission_error":      http.StatusForbidden,
		"not_found_error":       http.StatusNotFound,
		"invalid_request_error": http.StatusBadRequest,
		"api_error":             http.StatusBadGateway,
		"":                      http.StatusBadGateway,
	}
	for errorType, want := range cases {
		if got := (&zedFoldError{errorType: errorType}).status(); got != want {
			t.Fatalf("status(%q) = %d, want %d", errorType, got, want)
		}
	}
}

// TestZedHTTPErrorClassifiesForFastRetry pins the contract retry.go relies on:
// a Zed endpoint failure must report its upstream status, not a generic error.
func TestZedHTTPErrorClassifiesForFastRetry(t *testing.T) {
	failure := &zedHTTPError{operation: "discover Zed models", status: http.StatusServiceUnavailable, body: `{"message":"down"}`}
	var classified upstreamStatusError = failure
	if classified.upstreamStatus() != http.StatusServiceUnavailable {
		t.Fatalf("upstreamStatus() = %d", classified.upstreamStatus())
	}
	if !isFastRetryStatus(failure.upstreamStatus()) {
		t.Fatal("a 503 from Zed must be classified as fast-retryable")
	}
	if got := failure.Error(); got == "" || !isFastRetryStatus(failure.status) {
		t.Fatalf("Error() = %q", got)
	}
}

// TestZedBufferedResponseFlushIsANoop documents that services which flush after
// every event can write into the buffer used to fold a streaming response.
func TestZedBufferedResponseFlushIsANoop(t *testing.T) {
	buffered := newZedBufferedResponse()
	flusher, ok := any(buffered).(http.Flusher)
	if !ok {
		t.Fatal("zedBufferedResponse must satisfy http.Flusher")
	}
	flusher.Flush()
	if buffered.status != 0 || buffered.body.Len() != 0 {
		t.Fatalf("Flush changed the buffer: status=%d body=%q", buffered.status, buffered.body.String())
	}
}

// TestZedGeminiEstimateServesGoogleCountTokens covers the Google count_tokens
// path, which never reaches Zed because Zed cannot count for Google models.
func TestZedGeminiEstimateServesGoogleCountTokens(t *testing.T) {
	// The fake only guards against an accidental upstream call; a local estimate
	// must never reach it.
	cloud := newFakeZedCloud(t)
	gateway := startZedTestGateway(t)
	service := newZedGeminiService("local-key", gateway, nil)

	handler := func(key string, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		request.Header.Set("x-api-key", key)
		recorder := httptest.NewRecorder()
		service.handleCountTokens(recorder, request)
		return recorder
	}

	unauthorized := handler("wrong", `{"model":"gemini-test"}`)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("bad key status = %d, want 401", unauthorized.Code)
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
	get.Header.Set("Authorization", "Bearer local-key")
	method := httptest.NewRecorder()
	service.handleCountTokens(method, get)
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", method.Code)
	}
	ok := handler("local-key", `{"model":"gemini-test","messages":[{"role":"user","content":"hello there"}]}`)
	if ok.Code != http.StatusOK || gjson.Get(ok.Body.String(), "input_tokens").Int() <= 0 {
		t.Fatalf("status=%d body=%s", ok.Code, ok.Body.String())
	}
	if got := len(cloud.callsTo("/count_tokens")); got != 0 {
		t.Fatalf("Google count reached Zed: %d calls", got)
	}
}
