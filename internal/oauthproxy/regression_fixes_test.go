package oauthproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatToolChoiceNoneIsPreserved(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`"none"`),
		json.RawMessage(`{"type":"none"}`),
	} {
		choice, ok := chatToolChoice(raw)
		if !ok {
			t.Fatalf("chatToolChoice(%s) reported nothing to send", raw)
		}
		if choice != "none" {
			t.Fatalf("chatToolChoice(%s) = %v, want none", raw, choice)
		}
	}
}

func TestKimiDeviceTokenSlowDownGrowsInterval(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"error":"slow_down"}`)
	}))
	defer stub.Close()
	original := kimiTokenURL
	kimiTokenURL = stub.URL
	defer func() { kimiTokenURL = original }()

	token, next, err := exchangeKimiDeviceToken(context.Background(), stub.Client(), "device-id", nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "" {
		t.Fatalf("slow_down returned a token: %+v", token)
	}
	if next != 10*time.Second {
		t.Fatalf("slow_down next interval = %s, want 10s (5s + 5s step)", next)
	}
}

func TestMixedRuntimeSurfacesModelCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"object":"list","data":[]}`)
	}))
	defer upstream.Close()
	runtime, err := StartMixedProtocolAPIKeyRuntime(context.Background(), upstream.URL, "upstream-key",
		"chat-model,responses-model",
		map[string]string{"responses-model": "openai_responses", "chat-model": "openai"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	models := runtime.Models()
	if len(models) != 2 || models[0] != "chat-model" || models[1] != "responses-model" {
		t.Fatalf("runtime models = %v, want [chat-model responses-model]", models)
	}
}

func TestAnthropicPassthroughRecordsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/messages") || strings.Contains(request.URL.Path, "count_tokens") {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"msg_1","model":"claude-test","usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	defer upstream.Close()
	usage := NewUsageTracker()
	service := newAnthropicPassthroughService("local-key", upstream.URL, []string{"claude-test"}, &chatStaticAuthorizer{token: "upstream-key"}, usage)

	body := `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	service.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("passthrough status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	totals, ok := usage.Snapshot()
	if !ok || len(totals) != 1 {
		t.Fatalf("usage snapshot = %+v, want one model entry", totals)
	}
	if totals[0].Model != "claude-test" || totals[0].InputTokens != 11 || totals[0].OutputTokens != 7 {
		t.Fatalf("usage totals = %+v, want claude-test 11/7", totals[0])
	}
}

func TestPassthroughUsageScannerExtractsStreamTotals(t *testing.T) {
	scanner := &passthroughUsageScanner{}
	scanner.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":3,\"cache_creation_input_tokens\":2}}}\n\n"))
	// Split the delta across feeds to exercise the line buffer.
	scanner.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":"))
	scanner.feed([]byte("5}}\n\n"))

	if !scanner.sawUsage {
		t.Fatal("scanner did not observe usage events")
	}
	if scanner.model != "claude-test" {
		t.Fatalf("scanner model = %q, want claude-test", scanner.model)
	}
	if scanner.input != 9 || scanner.cacheRead != 3 || scanner.cacheWrite != 2 {
		t.Fatalf("scanner input/cache totals = %d/%d/%d, want 9/3/2", scanner.input, scanner.cacheRead, scanner.cacheWrite)
	}
	if scanner.output != 5 {
		t.Fatalf("scanner output = %d, want 5", scanner.output)
	}
}

func TestPassthroughUsageScannerWithoutStartUsage(t *testing.T) {
	// A gateway that omits message_start usage but reports delta usage must
	// still record the output total.
	scanner := &passthroughUsageScanner{}
	scanner.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\"}}\n\n"))
	scanner.feed([]byte("data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":4}}\n\n"))
	if !scanner.sawUsage {
		t.Fatal("scanner ignored delta usage when message_start had none")
	}
	if scanner.output != 4 {
		t.Fatalf("scanner output = %d, want 4", scanner.output)
	}

	// A stream with no usage events at all must not be flagged: recording it
	// would create a phantom zero-token entry in the usage tracker.
	empty := &passthroughUsageScanner{}
	empty.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\"}}\n\n"))
	empty.feed([]byte("data: {\"type\":\"message_delta\",\"delta\":{}}\n\n"))
	empty.flush()
	if empty.sawUsage {
		t.Fatal("scanner flagged usage for a stream that carried none")
	}
}

func TestPassthroughUsageScannerOutputHighWaterMark(t *testing.T) {
	// message_delta reports cumulative output_tokens; an out-of-order smaller
	// value must not shrink the total.
	scanner := &passthroughUsageScanner{}
	scanner.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n"))
	scanner.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"))
	if scanner.output != 7 {
		t.Fatalf("scanner output = %d, want high-water mark 7", scanner.output)
	}
}
