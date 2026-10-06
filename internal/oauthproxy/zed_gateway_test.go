package oauthproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// zedGatewayTestCatalog is one model per wire, so a request can be pointed at
// each data plane.
func zedGatewayTestCatalog() []zedModel {
	return []zedModel{
		{Provider: "anthropic", ID: "claude-sonnet-test"},
		{Provider: "open_ai", ID: "gpt-test"},
		{Provider: "x_ai", ID: "grok-test"},
		{Provider: "google", ID: "gemini-test"},
	}
}

// startZedTestGateway runs the real gateway against the fake cloud. The caller
// must have built the fake first: it is what redirects zedCloudBaseURL.
func startZedTestGateway(t *testing.T) *zedGateway {
	t.Helper()
	credential := &zedCredential{
		path: filepath.Join(t.TempDir(), zedTestCredent), fileName: zedTestCredent,
		auth:  zedUserAuth{userID: zedTestUserID, accessToken: zedTestToken, systemID: "sys-1"},
		login: "octo", organizationID: zedTestOrgID,
	}
	gateway, err := startZedGateway(t.Context(), newZedSession(credential), zedGatewayTestCatalog())
	if err != nil {
		t.Fatalf("startZedGateway() error: %v", err)
	}
	t.Cleanup(gateway.Stop)
	return gateway
}

func zedGatewayRequest(t *testing.T, gateway *zedGateway, method, path, key string, body string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, gateway.endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(raw)
}

func TestZedGatewayRefusesMalformedRequestsBeforeReachingZed(t *testing.T) {
	newFakeZedCloud(t)
	gateway := startZedTestGateway(t)
	working := `{"model":"claude-sonnet-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name       string
		method     string
		path       string
		key        string
		body       string
		wantStatus int
		wantError  string
	}{
		{"wrong gateway key", http.MethodPost, "/v1/messages", "not-the-key", working, http.StatusUnauthorized, "invalid gateway key"},
		{"unknown path", http.MethodPost, "/v1/unknown", gateway.key, working, http.StatusNotFound, "unsupported Zed gateway path"},
		{"wrong method", http.MethodGet, "/v1/messages", gateway.key, "", http.StatusMethodNotAllowed, "unsupported method"},
		{"missing model", http.MethodPost, "/v1/messages", gateway.key, `{"max_tokens":16}`, http.StatusBadRequest, "model is required"},
		{"unknown model", http.MethodPost, "/v1/messages", gateway.key, `{"model":"nope"}`, http.StatusNotFound, "not available on this Zed account"},
		{"model on the wrong wire", http.MethodPost, "/v1/messages", gateway.key, `{"model":"gpt-test"}`, http.StatusBadRequest, "not served over the anthropic protocol"},
	}
	for _, tc := range cases {
		response, body := zedGatewayRequest(t, gateway, tc.method, tc.path, tc.key, tc.body)
		if response.StatusCode != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d: %s", tc.name, response.StatusCode, tc.wantStatus, body)
		}
		if tc.wantError != "" && !strings.Contains(gjson.Get(body, "error.message").String(), tc.wantError) {
			t.Fatalf("%s: error body = %s", tc.name, body)
		}
		// A request the gateway refuses before it knows the wire is answered in
		// the Anthropic dialect, so the outer service can always read the message.
		if gjson.Get(body, "type").String() != "error" {
			t.Fatalf("%s: body is not an Anthropic error: %s", tc.name, body)
		}
	}
}

func TestZedGatewayWrapsAnthropicRequestsInZedEnvelope(t *testing.T) {
	cloud := newFakeZedCloud(t)
	gateway := startZedTestGateway(t)

	working := `{"model":"claude-sonnet-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	response, body := zedGatewayRequest(t, gateway, http.MethodPost, "/v1/messages", gateway.key, working)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	calls := cloud.callsTo("/completions")
	if len(calls) != 1 {
		t.Fatalf("completions calls = %d, want 1", len(calls))
	}
	envelope := calls[0].envelope
	if gjson.GetBytes(envelope, "provider").String() != "anthropic" || gjson.GetBytes(envelope, "model").String() != "claude-sonnet-test" {
		t.Fatalf("envelope = %s", envelope)
	}
	// Claude Code's metadata is dropped: the cloud owns streaming and betas.
	if calls[0].providerRequest().Get("stream").Exists() || calls[0].providerRequest().Get("metadata").Exists() {
		t.Fatalf("provider_request kept client-only fields: %s", calls[0].providerRequest().Raw)
	}
}

func TestZedWriteFailureSpeaksEachWiresErrorDialect(t *testing.T) {
	failure := zedCloudFailure{status: http.StatusTooManyRequests, code: "rate_limited", message: "slow down", retryAfter: "7"}
	cases := []struct {
		name     string
		wire     zedWire
		wantType string
		check    func(t *testing.T, body string)
	}{
		{"anthropic", zedWireAnthropic, "error", func(t *testing.T, body string) {
			if gjson.Get(body, "error.type").String() != "rate_limit_error" || gjson.Get(body, "error.message").String() != "slow down" {
				t.Fatalf("anthropic body = %s", body)
			}
		}},
		// The Responses and Chat dialects carry only the nested error object.
		{"responses", zedWireResponses, "", func(t *testing.T, body string) {
			if gjson.Get(body, "error.code").String() != "rate_limited" || gjson.Get(body, "error.type").String() != "rate_limit_error" {
				t.Fatalf("responses body = %s", body)
			}
		}},
		{"chat", zedWireChat, "", func(t *testing.T, body string) {
			if gjson.Get(body, "error.code").String() != "rate_limited" {
				t.Fatalf("chat body = %s", body)
			}
		}},
		{"google", zedWireGoogle, "", func(t *testing.T, body string) {
			if gjson.Get(body, "error.status").String() != "RESOURCE_EXHAUSTED" || gjson.Get(body, "error.code").Int() != 429 {
				t.Fatalf("google body = %s", body)
			}
			if gjson.Get(body, "error.message").String() != "slow down" {
				t.Fatalf("google message = %s", body)
			}
		}},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		zedWriteFailure(recorder, tc.wire, failure)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
			t.Fatalf("%s: status=%d retry-after=%q", tc.name, recorder.Code, recorder.Header().Get("Retry-After"))
		}
		if got := gjson.Get(recorder.Body.String(), "type").String(); got != tc.wantType {
			t.Fatalf("%s: top-level type = %q, want %q", tc.name, got, tc.wantType)
		}
		tc.check(t, recorder.Body.String())
	}
}

func TestZedWriteStreamFailureUsesTerminalEventDialect(t *testing.T) {
	failure := zedCloudFailure{status: http.StatusTooManyRequests, code: "rate_limited", message: "slow down"}
	cases := []struct {
		name      string
		wire      zedWire
		wantEvent string
		check     func(t *testing.T, payload string)
	}{
		{"anthropic names the event", zedWireAnthropic, "event: error\n", func(t *testing.T, payload string) {
			if gjson.Get(payload, "type").String() != "error" || gjson.Get(payload, "error.type").String() != "rate_limit_error" {
				t.Fatalf("anthropic payload = %s", payload)
			}
		}},
		{"responses names the event", zedWireResponses, "event: error\n", func(t *testing.T, payload string) {
			if gjson.Get(payload, "code").String() != "rate_limited" || gjson.Get(payload, "type").String() != "error" {
				t.Fatalf("responses payload = %s", payload)
			}
		}},
		{"chat is data-only", zedWireChat, "data: ", func(t *testing.T, payload string) {
			if gjson.Get(payload, "error.code").String() != "rate_limited" {
				t.Fatalf("chat payload = %s", payload)
			}
		}},
		{"google is data-only", zedWireGoogle, "data: ", func(t *testing.T, payload string) {
			if gjson.Get(payload, "error.status").String() != "RESOURCE_EXHAUSTED" {
				t.Fatalf("google payload = %s", payload)
			}
		}},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		if err := zedWriteStreamFailure(recorder, recorder, tc.wire, failure); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		record := recorder.Body.String()
		if !strings.HasPrefix(record, tc.wantEvent) || !strings.HasSuffix(record, "\n\n") {
			t.Fatalf("%s: record = %q", tc.name, record)
		}
		tc.check(t, zedEventPayload(t, record))
	}
}

// zedEventPayload extracts the JSON of one SSE record.
func zedEventPayload(t *testing.T, record string) string {
	t.Helper()
	for line := range strings.SplitSeq(record, "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			return payload
		}
	}
	t.Fatalf("record has no data line: %q", record)
	return ""
}

func TestZedGoogleStatusMapsHTTPStatuses(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "INVALID_ARGUMENT",
		http.StatusUnauthorized:        "UNAUTHENTICATED",
		http.StatusForbidden:           "PERMISSION_DENIED",
		http.StatusNotFound:            "NOT_FOUND",
		http.StatusTooManyRequests:     "RESOURCE_EXHAUSTED",
		http.StatusServiceUnavailable:  "UNAVAILABLE",
		http.StatusInternalServerError: "INTERNAL",
		529:                            "INTERNAL",
	}
	for status, want := range cases {
		if got := zedGoogleStatus(status); got != want {
			t.Fatalf("zedGoogleStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestZedFailureFromErrorNormalizesTokenErrors(t *testing.T) {
	rejected := zedFailureFromError(&zedHTTPError{
		operation: "create Zed LLM token", status: http.StatusUnauthorized,
		body: `{"message":"token expired"}`, retryAfter: "5",
	})
	if rejected.status != http.StatusUnauthorized || rejected.retryAfter != "5" {
		t.Fatalf("token failure = %+v", rejected)
	}
	if !strings.Contains(rejected.message, "ccl oauth zed") || !strings.Contains(rejected.message, "token expired") {
		t.Fatalf("token failure message = %q", rejected.message)
	}

	transport := zedFailureFromError(errZedTestTransport)
	if transport.status != http.StatusBadGateway || transport.message != errZedTestTransport.Error() {
		t.Fatalf("transport failure = %+v", transport)
	}
}

type zedTestError string

func (e zedTestError) Error() string { return string(e) }

const errZedTestTransport = zedTestError("dial tcp: connection refused")

func TestZedBufferedResponseRelaysErrorsAndFoldsStreams(t *testing.T) {
	// A non-2xx pass-through keeps the upstream status and its hints.
	buffered := newZedBufferedResponse()
	buffered.Header().Set("Content-Type", "application/json")
	buffered.Header().Set("Retry-After", "9")
	buffered.WriteHeader(http.StatusPaymentRequired)
	_, _ = buffered.Write([]byte(`{"error":{"message":"pay up"}}`))

	recorder := httptest.NewRecorder()
	buffered.writeFolded(recorder)
	if recorder.Code != http.StatusPaymentRequired || recorder.Header().Get("Retry-After") != "9" ||
		gjson.Get(recorder.Body.String(), "error.message").String() != "pay up" {
		t.Fatalf("relayed status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// A successful stream folds into one Anthropic Message.
	buffered = newZedBufferedResponse()
	_, _ = buffered.Write([]byte(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")))
	recorder = httptest.NewRecorder()
	buffered.writeFolded(recorder)
	if recorder.Code != http.StatusOK || gjson.Get(recorder.Body.String(), "content.0.text").String() != "Hi" {
		t.Fatalf("folded status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gjson.Get(recorder.Body.String(), "usage.input_tokens").Int() != 3 {
		t.Fatalf("folded usage = %s", recorder.Body.String())
	}

	// An error event inside the stream becomes the folded response's error.
	buffered = newZedBufferedResponse()
	buffered.Header().Set("Content-Type", "text/event-stream")
	_, _ = buffered.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"))
	recorder = httptest.NewRecorder()
	buffered.writeFolded(recorder)
	if recorder.Code != 529 || gjson.Get(recorder.Body.String(), "error.message").String() != "busy" {
		t.Fatalf("stream error status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestZedBufferedResponseRefusesOversizedBodies(t *testing.T) {
	buffered := newZedBufferedResponse()
	if _, err := buffered.Write(make([]byte, 1024)); err != nil {
		t.Fatalf("small write: %v", err)
	}
	if _, err := buffered.Write(make([]byte, anthropicAssemblerMaxRetainedBytes)); err == nil {
		t.Fatal("oversized write was accepted")
	}
}

func TestZedDecodeFieldsRejectsNonObjectBodies(t *testing.T) {
	if _, err := zedDecodeFields([]byte(`[1,2]`)); err == nil {
		t.Fatal("array body was accepted")
	}
	if _, err := zedDecodeFields([]byte(`not json`)); err == nil {
		t.Fatal("non-JSON body was accepted")
	}
	fields, err := zedDecodeFields([]byte(`{"model":"m"}`))
	if err != nil || string(fields["model"]) != `"m"` {
		t.Fatalf("fields = %v err = %v", fields, err)
	}
	zedSetField(fields, "stream", true)
	if !json.Valid(fields["stream"]) || string(fields["stream"]) != "true" {
		t.Fatalf("zedSetField wrote %s", fields["stream"])
	}
}
