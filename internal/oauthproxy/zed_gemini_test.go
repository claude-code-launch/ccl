package oauthproxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// zedGeminiTestService runs the Google data plane against a stub gateway, so
// each of its own guards can be reached without driving a whole Zed runtime.
func zedGeminiTestService(t *testing.T, handler http.HandlerFunc, usage *UsageTracker) *zedGeminiService {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return newZedGeminiService("local-key", &zedGateway{endpoint: server.URL, key: "gateway-key"}, usage)
}

func zedGeminiHandler(service *zedGeminiService, key, method, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/messages", strings.NewReader(body))
	if key != "" {
		request.Header.Set("x-api-key", key)
	}
	recorder := httptest.NewRecorder()
	service.handleMessages(recorder, request)
	return recorder
}

const zedGeminiStreamBody = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}` + "\n\n"

func TestZedGeminiServiceRefusesRequestsBeforeCallingTheGateway(t *testing.T) {
	var calls int
	service := zedGeminiTestService(t, func(http.ResponseWriter, *http.Request) { calls++ }, nil)

	if recorder := zedGeminiHandler(service, "", http.MethodPost, `{"model":"gemini-test"}`); recorder.Code != http.StatusUnauthorized ||
		gjson.Get(recorder.Body.String(), "error.type").String() != "authentication_error" {
		t.Fatalf("unauthenticated = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := zedGeminiHandler(service, "local-key", http.MethodGet, ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", recorder.Code)
	}
	if recorder := zedGeminiHandler(service, "local-key", http.MethodPost, ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", recorder.Code)
	}
	// No model and no messages: the converter refuses before the gateway is used.
	if recorder := zedGeminiHandler(service, "local-key", http.MethodPost, `{"messages":[]}`); recorder.Code != http.StatusBadRequest ||
		gjson.Get(recorder.Body.String(), "error.type").String() != "invalid_request_error" {
		t.Fatalf("unconvertible body = %d %s", recorder.Code, recorder.Body.String())
	}
	if calls != 0 {
		t.Fatalf("a rejected request reached the gateway: %d calls", calls)
	}
}

func TestZedGeminiServiceRelaysGatewayFailuresAndHonorsRetryAfter(t *testing.T) {
	seen := make(chan string, 1)
	service := zedGeminiTestService(t, func(writer http.ResponseWriter, request *http.Request) {
		select {
		case seen <- request.Header.Get("Authorization"):
		default:
		}
		writer.Header().Set("Retry-After", "11")
		writer.WriteHeader(http.StatusPaymentRequired)
		_, _ = writer.Write([]byte(`{"message":"upgrade your plan"}`))
	}, nil)

	recorder := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusPaymentRequired || recorder.Header().Get("Retry-After") != "11" {
		t.Fatalf("status=%d retry-after=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if got := gjson.Get(recorder.Body.String(), "error.type").String(); got != "payment_required" && got != "invalid_request_error" {
		t.Fatalf("error type = %q in %s", got, recorder.Body.String())
	}
	if !strings.Contains(gjson.Get(recorder.Body.String(), "error.message").String(), "upgrade your plan") {
		t.Fatalf("message was dropped: %s", recorder.Body.String())
	}
	if key := <-seen; key != "Bearer gateway-key" {
		t.Fatalf("gateway authorization = %q", key)
	}
}

func TestZedGeminiServiceRetriesTheGatewayLikeEveryOuterService(t *testing.T) {
	previous := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = previous })

	var calls int
	service := zedGeminiTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(zedGeminiStreamBody))
	}, nil)

	recorder := zedGeminiHandler(service, "local-key", http.MethodPost, `{"model":"gemini-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"Hello"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls != 2 {
		t.Fatalf("gateway attempts = %d, want the retry to have run", calls)
	}
}

func TestZedGeminiServiceStreamsAndFoldsLikeTheOtherPlanes(t *testing.T) {
	usage := NewUsageTracker()
	service := zedGeminiTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(zedGeminiStreamBody))
	}, usage)

	streamed := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if streamed.Code != http.StatusOK || !strings.Contains(streamed.Body.String(), "event: message_stop") ||
		!strings.Contains(streamed.Body.String(), `"text":"Hello"`) {
		t.Fatalf("stream status=%d body=%s", streamed.Code, streamed.Body.String())
	}
	totals, recorded := usage.Snapshot()
	if !recorded || len(totals) != 1 || totals[0].Model != "gemini-test" || totals[0].Requests != 1 {
		t.Fatalf("usage = %+v (recorded=%v)", totals, recorded)
	}
	if totals[0].InputTokens <= 0 || totals[0].OutputTokens <= 0 {
		t.Fatalf("streamed tokens were not counted: %+v", totals[0])
	}

	folded := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if folded.Code != http.StatusOK || gjson.Get(folded.Body.String(), "content.0.text").String() != "Hello" {
		t.Fatalf("folded status=%d body=%s", folded.Code, folded.Body.String())
	}
}

func TestZedGeminiServiceReportsAStreamThatEndsEarly(t *testing.T) {
	// A stream with no finishReason is a truncated response, not a success.
	service := zedGeminiTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}` + "\n\n"))
	}, nil)

	streamed := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := streamed.Body.String()
	if streamed.Code != http.StatusOK || !strings.Contains(body, "event: error") || !strings.Contains(body, "api_error") {
		t.Fatalf("stream status=%d body=%s", streamed.Code, body)
	}

	folded := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","messages":[{"role":"user","content":"hi"}]}`)
	if folded.Code != http.StatusBadGateway ||
		gjson.Get(folded.Body.String(), "error.type").String() != "api_error" {
		t.Fatalf("non-stream status=%d body=%s", folded.Code, folded.Body.String())
	}
}

// zedFailingWriter stands in for a client that hung up mid-stream.
type zedFailingWriter struct{ header http.Header }

func (w *zedFailingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *zedFailingWriter) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func (w *zedFailingWriter) WriteHeader(int) {}

func (w *zedFailingWriter) Flush() {}

func TestZedGeminiServiceStopsWhenTheClientGoesAway(t *testing.T) {
	var calls int
	service := zedGeminiTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(zedGeminiStreamBody))
	}, nil)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gemini-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "local-key")
	service.handleMessages(&zedFailingWriter{}, request)

	// The write failure is the end of the request, not a retry.
	if calls != 1 {
		t.Fatalf("gateway calls = %d, want 1", calls)
	}
}

func TestZedGeminiServiceRefusesOversizedMessageBodies(t *testing.T) {
	service := zedGeminiTestService(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an oversized request must never reach the gateway")
	}, nil)
	recorder := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","padding":"`+strings.Repeat("x", int(chatMaxBodyBytes))+`","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestZedGeminiServiceReportsAnUnreachableGateway(t *testing.T) {
	previous := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = previous })

	// A listener that is already closed: the gateway is unreachable, which the
	// outer service reports as a 502 rather than hanging.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	service := newZedGeminiService("local-key", &zedGateway{endpoint: dead.URL, key: "gateway-key"}, nil)

	recorder := zedGeminiHandler(service, "local-key", http.MethodPost,
		`{"model":"gemini-test","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "api_error" || !strings.Contains(payload.Error.Message, "gateway") {
		t.Fatalf("error = %+v", payload.Error)
	}
}

func TestZedGeminiServiceCountRefusesOversizedBodies(t *testing.T) {
	service := zedGeminiTestService(t, func(http.ResponseWriter, *http.Request) {
		t.Error("count_tokens must never reach the gateway")
	}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(strings.Repeat("x", int(chatMaxBodyBytes)+1)))
	request.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	service.handleCountTokens(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("body is not JSON: %s", recorder.Body.String())
	}
}
