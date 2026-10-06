package oauthproxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// startZedStubGateway runs the real gateway against a stubbed cloud, for the
// failure paths that need control over the cloud's answers.
func startZedStubGateway(t *testing.T) *zedGateway {
	t.Helper()
	credential := zedTestCredentialObject()
	credential.organizationID = zedTestOrgID
	session := newZedSession(credential)
	session.organizationID, session.profileChecked = zedTestOrgID, true
	gateway, err := startZedGateway(t.Context(), session, zedGatewayTestCatalog())
	if err != nil {
		t.Fatalf("startZedGateway() error: %v", err)
	}
	t.Cleanup(gateway.Stop)
	return gateway
}

// TestZedGatewayRefusesMalformedJSONOnEveryWire pins the boundary between
// gjson's leniency and Go's decoder: gjson can still find the model in a broken
// body, so the wire builders must be the ones to refuse it.
func TestZedGatewayRefusesMalformedJSONOnEveryWire(t *testing.T) {
	newFakeZedCloud(t)
	gateway := startZedStubGateway(t)

	for _, tc := range []struct{ path, model string }{
		{"/v1/messages", "claude-sonnet-test"},
		{"/v1/responses", "gpt-test"},
		{"/v1/chat/completions", "grok-test"},
		{"/v1/google", "gemini-test"},
	} {
		response, body := zedGatewayRequest(t, gateway, http.MethodPost, tc.path, gateway.key,
			`{"model":"`+tc.model+`","broken":}`)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(gjson.Get(body, "error.message").String(), "invalid request JSON") {
			t.Fatalf("%s: status=%d body: %s", tc.path, response.StatusCode, body)
		}
	}
}

func TestZedGatewayRelaysCredentialFailuresInTheAnthropicDialect(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"message":"expired"}`))
	})
	gateway := startZedStubGateway(t)

	response, body := zedGatewayRequest(t, gateway, http.MethodPost, "/v1/messages", gateway.key,
		`{"model":"claude-sonnet-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.StatusCode, body)
	}
	if message := gjson.Get(body, "error.message").String(); !strings.Contains(message, "ccl oauth zed") {
		t.Fatalf("message = %q, want the re-authenticate instruction", message)
	}
}

func TestZedGatewayRefusesATokenCountWithoutTokens(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/count_tokens" {
			writeTestJSON(writer, map[string]any{"unexpected": true})
			return
		}
		writeTestJSON(writer, map[string]any{"token": "llm-stub"})
	})
	gateway := startZedStubGateway(t)

	response, body := zedGatewayRequest(t, gateway, http.MethodPost, "/v1/messages/count_tokens", gateway.key,
		`{"model":"claude-sonnet-test","messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(gjson.Get(body, "error.message").String(), "no tokens") {
		t.Fatalf("status=%d body: %s", response.StatusCode, body)
	}
}

// TestZedGatewayStreamsBlankLinesAndStopsWhenTheClientLeaves covers the two
// scanner paths a client drop can take: an empty keep-alive line is skipped,
// and a write failure ends the request without a phantom stream error.
func TestZedGatewayStreamsBlankLinesAndStopsWhenTheClientLeaves(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/completions" {
			writeTestJSON(writer, map[string]any{"token": "llm-stub"})
			return
		}
		writer.Header().Set(zedServerStatusHeader, "true")
		_, _ = writer.Write([]byte(ndjson(
			``,
			`{"status":"started"}`,
			``,
			`{"event":{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}}`,
			``,
			`{"event":{"type":"message_stop"}}`,
		)))
	})
	gateway := startZedStubGateway(t)

	response, body := zedGatewayRequest(t, gateway, http.MethodPost, "/v1/messages", gateway.key,
		`{"model":"claude-sonnet-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	if strings.Contains(body, "stream_ended_unexpectedly") {
		t.Fatalf("a keep-alive-only line ended the stream early: %s", body)
	}

	// The same stream into a writer that fails: the request just ends.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gateway.endpoint+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+gateway.key)
	recorder := &zedFailingWriter{}
	gateway.serveHTTP(recorder, request)
}

func TestParseZedStatusMapsFailedCodesToStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"rate limit", `{"failed":{"code":"rate_limit_exceeded","message":"slow"}}`, http.StatusTooManyRequests},
		{"overloaded", `{"failed":{"code":"overloaded","message":"busy"}}`, 529},
		{"payment", `{"failed":{"code":"payment_required","message":"pay"}}`, http.StatusPaymentRequired},
		{"anything else keeps the gateway status", `{"failed":{"code":"internal","message":"boom"}}`, http.StatusBadGateway},
	} {
		line := parseZedStatus(gjson.Parse(tc.raw))
		if line.status != "failed" || line.failure == nil || line.failure.status != tc.want {
			t.Fatalf("%s: %+v", tc.name, line)
		}
	}
	// A status that is neither a string nor an object carries nothing.
	for _, raw := range []string{`{"status":7}`, `{"status":null}`, `{"status":[1]}`} {
		if line := parseZedStatus(gjson.Parse(raw)); line.status != "" || line.event != nil || line.failure != nil {
			t.Fatalf("%s parsed as %+v", raw, line)
		}
	}
}

func TestZedDecodeFieldsRefusesJSONNull(t *testing.T) {
	if _, err := zedDecodeFields([]byte(`null`)); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("null body error = %v", err)
	}
}
