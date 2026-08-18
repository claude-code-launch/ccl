package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConvertAnthropicToChatCompletionsToolRoundTrip(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test[1m]","max_tokens":100,"stream":true,
		"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},
		"system":"be concise",
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"find it"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
		]
	}`)
	converted, err := convertAnthropicToChatCompletions(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gpt-test" || body["stream"] != true {
		t.Fatalf("wire body flags = %+v", body)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	streamOptions, _ := body["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %v", body["stream_options"])
	}
	messages := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4 (system+user+assistant+tool)", len(messages))
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" {
		t.Fatalf("first message = %+v, want system", system)
	}
	assistant := messages[2].(map[string]any)
	toolCalls, _ := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("assistant tool_calls = %+v", assistant)
	}
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "lookup" || function["arguments"] != `{"q":"x"}` {
		t.Fatalf("function = %+v", function)
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "done" {
		t.Fatalf("tool message = %+v", toolMsg)
	}
}

func TestConvertAnthropicToChatCompletionsReasoningEffort(t *testing.T) {
	cases := []struct {
		thinking map[string]any
		want     string
	}{
		{map[string]any{"type": "disabled"}, ""},
		{map[string]any{"type": "enabled", "budget_tokens": 1000}, "low"},
		{map[string]any{"type": "enabled", "budget_tokens": 8000}, "medium"},
		{map[string]any{"type": "enabled", "budget_tokens": 20000}, "high"},
		{map[string]any{"type": "enabled", "budget_tokens": 64000}, "xhigh"},
	}
	for _, tc := range cases {
		payload := map[string]any{
			"model": "gpt-test", "max_tokens": 32,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}
		if tc.thinking != nil {
			payload["thinking"] = tc.thinking
		}
		raw, _ := json.Marshal(payload)
		converted, err := convertAnthropicToChatCompletions(raw)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(converted.body, &body); err != nil {
			t.Fatal(err)
		}
		got, _ := body["reasoning_effort"].(string)
		if got != tc.want {
			t.Fatalf("thinking=%v reasoning_effort=%q, want %q", tc.thinking, got, tc.want)
		}
	}
}

func TestProcessChatCompletionsStream(t *testing.T) {
	request := &anthropicAdapterRequest{clientModel: "gpt-test", thinkingEnabled: true}
	assembler := newAnthropicResponseAssembler(request, nil)
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":""},"index":0,"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"thinking..."},"index":0,"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"answer"},"index":0,"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"index":0,"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"index":0,"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	if err := processChatCompletionsStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatal(err)
	}
	response := assembler.response()
	blocks := response["content"].([]kiroResponseBlock)
	var thinking, text, toolName string
	var toolInput map[string]any
	for _, block := range blocks {
		switch block.Type {
		case "thinking":
			thinking = block.Thinking
		case "text":
			text = block.Text
		case "tool_use":
			toolName = block.Name
			if block.Input != nil {
				toolInput = *block.Input
			}
		}
	}
	if thinking != "thinking..." || text != "answer" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
	if toolName != "lookup" || toolInput["q"] != "x" {
		t.Fatalf("tool name=%q input=%v", toolName, toolInput)
	}
	if response["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", response["stop_reason"])
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != 9 || usage["output_tokens"] != 4 || usage["cache_read_input_tokens"] != 3 {
		t.Fatalf("usage=%v", usage)
	}
}

func TestOpenAIChatEndToEndStreaming(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"index\":0,\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"index\":0,\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	body := postClaudeMessage(t, context.Background(), runtime, "gpt-test")
	for _, want := range []string{"message_start", "text_delta", "hello", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

func TestOpenAIChatEndToEndNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"non-stream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	payload := strings.NewReader(`{"model":"gpt-test","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, runtime.Endpoint()+"/messages", payload)
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("non-stream")) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

func TestOpenAIChatErrorMapping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "30")
		writer.Header().Set("X-Request-Id", "req-1")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	payload := strings.NewReader(`{"model":"gpt-test","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", payload)
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if response.Header.Get("Retry-After") != "30" || response.Header.Get("X-Request-Id") != "req-1" {
		t.Fatalf("forwarded headers = %+v", response.Header)
	}
	if !strings.Contains(string(body), `"type":"rate_limit_error"`) {
		t.Fatalf("error body = %s", body)
	}
}
