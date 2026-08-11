package oauthproxy

import (
	"bytes"
	"context"
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
	"time"
)

func TestCodexResponsesOAuthRefreshesOnlyAfter401(t *testing.T) {
	var upstreamCalls atomic.Int32
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("refresh_token") != "refresh-old" {
				t.Errorf("refresh_token = %q", request.Form.Get("refresh_token"))
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`)
		case "/codex/responses":
			upstreamCalls.Add(1)
			if request.Header.Get("Authorization") == "Bearer access-old" {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(writer, `{"error":{"message":"expired"}}`)
				return
			}
			if request.Header.Get("Authorization") != "Bearer access-new" {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("Chatgpt-Account-Id") != "account-1" {
				t.Errorf("account header = %q", request.Header.Get("Chatgpt-Account-Id"))
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_oauth\",\"model\":\"gpt-test\"}}\n\n")
			_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"refreshed\"}\n\n")
			_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_oauth\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	previousBase, previousToken := codexOAuthBaseURL, codexOAuthTokenURL
	codexOAuthBaseURL, codexOAuthTokenURL = server.URL+"/codex", server.URL+"/token"
	t.Cleanup(func() { codexOAuthBaseURL, codexOAuthTokenURL = previousBase, previousToken })
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(authDir, "codex-refresh.json")
	if err := os.WriteFile(credentialPath, []byte(`{"type":"codex","access_token":"access-old","refresh_token":"refresh-old","account_id":"account-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := StartOAuth(context.Background(), ProviderCodex, "gpt-test", filepath.Base(credentialPath))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	body := postClaudeMessage(t, context.Background(), runtime, "gpt-test")
	if !strings.Contains(body, "refreshed") {
		t.Fatalf("response = %s", body)
	}
	if upstreamCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d refresh calls=%d", upstreamCalls.Load(), refreshCalls.Load())
	}
	stored, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`"access_token": "access-new"`)) || !bytes.Contains(stored, []byte(`"refresh_token": "refresh-new"`)) {
		t.Fatalf("refreshed credential was not persisted: %s", stored)
	}
}

func TestCodexResponsesDoesNotRewrite403As503(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, `{"error":{"message":"client version rejected"}}`)
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	payload := strings.NewReader(`{"model":"gpt-test","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", payload)
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d; want one 403", response.StatusCode, calls.Load())
	}
}

func TestCodexResponsesReasoningAndUsageSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning\",\"model\":\"gpt-test\"}}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"enc_signature\"}}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reasoning\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n")
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	body := postClaudeMessage(t, context.Background(), runtime, "gpt-test")
	for _, want := range []string{"thinking_delta", "enc_signature", "answer", `"input_tokens":9`, `"cache_read_input_tokens":3`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
}

func TestConvertAnthropicToCodexResponsesToolRoundTrip(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test[1m]","max_tokens":100,"stream":true,
		"metadata":{"user_id":"user_session_123e4567-e89b-12d3-a456-426614174000"},
		"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},
		"system":"be concise",
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"find it"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
		]
	}`)
	converted, err := convertAnthropicToCodexResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gpt-test" || body["stream"] != true || body["store"] != false {
		t.Fatalf("wire body flags = %+v", body)
	}
	if body["prompt_cache_key"] != "user_session_123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("prompt_cache_key = %v", body["prompt_cache_key"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %+v", reasoning)
	}
	encoded := string(converted.body)
	for _, want := range []string{`"role":"developer"`, `"type":"function_call"`, `"type":"function_call_output"`, `"type":"function"`} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("wire body missing %s: %s", want, encoded)
		}
	}
}

func TestCodexResponsesNonStreamingClaudeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// A compatible gateway may ignore stream=true and return one Responses
		// object. The CCL adapter accepts both wire forms.
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_nonstream","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"non-stream"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
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
