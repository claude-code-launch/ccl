package oauthproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func qoderCapacityEnvelope(queued bool) string {
	queue, _ := json.Marshal(map[string]any{"isQueued": queued, "modelKey": "qfmodel", "queueCount": 0, "queueType": "p1", "retryAfterSeconds": 2, "serviceAvailable": true, "waitTime": 0})
	inner, _ := json.Marshal(map[string]any{"code": "10605", "subCode": "RESOURCE_LIMIT", "message": string(queue)})
	outer, _ := json.Marshal(map[string]any{"code": "403", "message": string(inner)})
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": string(outer)})
	return "data: " + string(envelope) + "\n\n"
}

type qoderCapacityTransport struct {
	body  string
	calls atomic.Int32
}

func (transport *qoderCapacityTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(transport.body))}, nil
}

func qoderCapacityService(t *testing.T, body string) (*qoderService, *qoderCapacityTransport) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qoder-fixture.json"), []byte(`{"type":"qoder","access_token":"fake-token","user_id":"fake-user","machine_id":"fake-machine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &qoderCapacityTransport{body: body}
	client := &http.Client{Transport: transport}
	pool := newQoderCredentialPool(dir, "qoder-fixture.json")
	pool.client = client // Even an unexpected refresh must never reach a real provider.
	return &qoderService{apiKey: "fake-local-key", models: []qoderModel{{ID: "qfmodel"}}, pool: pool, client: client}, transport
}

func callQoderCapacityFixture(service *qoderService, stream bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"qfmodel","stream":%t,"messages":[{"role":"user","content":"fixture"}]}`, stream)))
	request.Header.Set("X-Api-Key", service.apiKey)
	writer := httptest.NewRecorder()
	service.handleMessages(writer, request)
	return writer
}

func TestQoderCapacityBeforeOutputReturnsRetryableHTTPError(t *testing.T) {
	for _, queued := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("queued=%t/stream=%t", queued, stream), func(t *testing.T) {
				// Empty/usage frames are not assistant output and must not commit 200.
				service, transport := qoderCapacityService(t, "data: {\"body\":{\"usage\":{\"prompt_tokens\":20}}}\n\n"+qoderCapacityEnvelope(queued))
				writer := callQoderCapacityFixture(service, stream)
				body := writer.Body.String()
				if writer.Code != 429 || writer.Header().Get("Retry-After") != "2" || !strings.Contains(body, "rate_limit_error") || !strings.Contains(body, "retry after 2s") {
					t.Fatalf("capacity error lost retry semantics: status=%d headers=%v body=%s", writer.Code, writer.Header(), body)
				}
				if strings.Contains(body, "message_start") || strings.Contains(body, "10605") || strings.Contains(body, "RESOURCE_LIMIT") {
					t.Fatalf("capacity rejection was streamed as success or leaked its nested wire encoding: %s", body)
				}
				if transport.calls.Load() != 1 {
					t.Fatal("stream capacity error triggered hidden upstream retries")
				}
			})
		}
	}
}

func TestQoderCapacityAfterOutputDoesNotReplayPartialAnswer(t *testing.T) {
	content := "data: {\"body\":{\"choices\":[{\"delta\":{\"content\":\"partial-answer\"}}]}}\n\n"
	service, transport := qoderCapacityService(t, content+qoderCapacityEnvelope(false))
	writer := callQoderCapacityFixture(service, true)
	body := writer.Body.String()
	if writer.Code != 200 || !strings.Contains(body, "partial-answer") || !strings.Contains(body, "rate_limit_error") || !strings.Contains(body, `"retry_after_seconds":2`) || strings.Contains(body, "message_stop") || strings.Contains(body, "RESOURCE_LIMIT") {
		t.Fatalf("partial stream was reported incorrectly: %d %s", writer.Code, body)
	}
	if transport.calls.Load() != 1 {
		t.Fatal("partial output was silently replayed")
	}
}

func TestQoderPermissionErrorIsNotCapacity(t *testing.T) {
	service, transport := qoderCapacityService(t, "data: {\"statusCodeValue\":403,\"body\":{\"message\":\"permission denied\",\"isQueued\":false}}\n\n")
	writer := callQoderCapacityFixture(service, true)
	if writer.Code != 403 || writer.Header().Get("Retry-After") != "" || !strings.Contains(writer.Body.String(), "permission_error") || transport.calls.Load() != 1 {
		t.Fatalf("permission denial was treated as retryable capacity: %d %s", writer.Code, writer.Body.String())
	}
}
