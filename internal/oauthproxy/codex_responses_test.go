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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiktoken-go/tokenizer"
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

func TestCodexResponsesCountTokensUsesCodexTokenizer(t *testing.T) {
	runtime, err := StartOpenAIResponsesAPI(context.Background(), "http://127.0.0.1:1/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	content := strings.Repeat(`if err != nil { return fmt.Errorf("item[%d]: %w", index, err) }\n`, 200)
	payload, err := json.Marshal(map[string]any{
		"model": "gpt-test", "max_tokens": 32,
		"metadata": map[string]any{"user_id": "user_session_123e4567-e89b-12d3-a456-426614174000"},
		"messages": []map[string]any{{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages/count_tokens", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}

	converted, err := convertAnthropicToCodexResponses(payload)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	want, err := codec.Count(string(converted.body))
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != want {
		t.Fatalf("input_tokens=%d, want Codex tokenizer count %d", result.InputTokens, want)
	}
	if result.InputTokens == estimateApproxTokensBytes(payload) {
		t.Fatalf("input_tokens still uses the old rune estimate: %d", result.InputTokens)
	}
}

func TestCodexCompactionRequestTrimsOldestHistoryOnly(t *testing.T) {
	messages := make([]map[string]any, 0, 24)
	for index := 0; index < 24; index++ {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": fmt.Sprintf("history-%02d %s", index, strings.Repeat("const value = object[index]; ", 80)),
		})
	}
	payload, err := json.Marshal(map[string]any{
		"model": "gpt-test", "max_tokens": 1024,
		"system":        "You are a helpful AI assistant tasked with summarizing conversations.",
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "xhigh"},
		"tools": []map[string]any{{
			"name": "unused_during_compaction", "description": "unused",
			"input_schema": map[string]any{"type": "object"},
		}},
		"metadata": map[string]any{"user_id": "user_session_123e4567-e89b-12d3-a456-426614174000"},
		"messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	converted, err := convertAnthropicToCodexResponses(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !converted.compaction {
		t.Fatal("Claude Code compaction system prompt was not detected")
	}
	var wire map[string]any
	if err := json.Unmarshal(converted.body, &wire); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := wire["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || reasoning["summary"] != nil {
		t.Fatalf("compaction reasoning = %+v, want low without reasoning summary", reasoning)
	}
	if wire["tools"] != nil || wire["parallel_tool_calls"] != false {
		t.Fatalf("compaction retained unnecessary tools: %+v", wire["tools"])
	}
	originalTokens, err := countCodexResponsesInputTokens(converted.body)
	if err != nil {
		t.Fatal(err)
	}
	trimmed, stats, err := trimCodexCompactionBody(converted.body, originalTokens/2)
	if err != nil {
		t.Fatal(err)
	}
	targetTokens := originalTokens / 2
	if stats.droppedItems == 0 || stats.finalTokens > targetTokens {
		t.Fatalf("trim stats = %+v, target=%d", stats, originalTokens/2)
	}
	if stats.finalTokens < targetTokens*3/4 {
		t.Fatalf("trim discarded too much recent context: stats=%+v target=%d", stats, targetTokens)
	}
	if stats.tokenizerPasses > 5 {
		t.Fatalf("trim used %d tokenizer passes, want at most 5", stats.tokenizerPasses)
	}
	text := string(trimmed)
	if strings.Contains(text, "history-00") {
		t.Fatal("oldest history was retained")
	}
	if !strings.Contains(text, "history-23") || !strings.Contains(text, codexCompactionTruncationNotice) {
		t.Fatal("newest history or truncation notice was lost")
	}
	if !strings.Contains(text, "tasked with summarizing conversations") {
		t.Fatal("summarizer system message was lost")
	}
}

func TestCodexCompactionDetectsClaudeCodeUserPrompt(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.6-sol[1m]","max_tokens":4096,"stream":true,
		"system":"You are Claude Code.",
		"messages":[
			{"role":"user","content":"ordinary work"},
			{"role":"user","content":"Your task is to create a detailed summary of this conversation. This summary will be placed at the start of a continuing session; newer messages that build on this context will follow after your summary."}
		]
	}`)
	converted, err := convertAnthropicToCodexResponses(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !converted.compaction {
		t.Fatal("Claude Code 2.1.223 user-message compact prompt was not detected")
	}
	if converted.model != "gpt-5.6-sol" {
		t.Fatalf("selected model changed: %q", converted.model)
	}
	if converted.compactionReason != "user_detailed_summary" {
		t.Fatalf("legacy compact reason = %q signals=%q", converted.compactionReason, converted.compactionSignals)
	}

	structured := []byte(`{
		"model":"gpt-5.6-sol","max_tokens":4096,"stream":false,
		"system":"You are Claude Code.",
		"messages":[{"role":"user","content":"Your summary should include the following sections:\n1. Primary Request and Intent: preserve the user's intent.\nThere may be additional summarization instructions provided in the included context.\nREMINDER: Do NOT call any tools. Respond with plain text only."}]
	}`)
	converted, err = convertAnthropicToCodexResponses(structured)
	if err != nil {
		t.Fatal(err)
	}
	if !converted.compaction || converted.compactionReason != "user_structured_summary" {
		t.Fatalf("structured compact classification = %v reason=%q signals=%q",
			converted.compaction, converted.compactionReason, converted.compactionSignals)
	}
	for _, signal := range []string{"summary_sections", "primary_request", "plain_text_only", "additional_instructions", "no_tools"} {
		if !strings.Contains(converted.compactionSignals, signal) {
			t.Fatalf("structured compact signals %q missing %q", converted.compactionSignals, signal)
		}
	}

	normal := []byte(`{
		"model":"gpt-5.6-sol","max_tokens":32,
		"messages":[{"role":"user","content":"How should an app summarize this conversation?"}]
	}`)
	converted, err = convertAnthropicToCodexResponses(normal)
	if err != nil {
		t.Fatal(err)
	}
	if converted.compaction {
		t.Fatal("ordinary summarization discussion was classified as compaction")
	}
}

func TestCodexCompactionBuffersSSEOverflow(t *testing.T) {
	converted, err := convertAnthropicToCodexResponses([]byte(`{
		"model":"gpt-test","max_tokens":32,"stream":true,
		"system":"You are a helpful AI assistant tasked with summarizing conversations.",
		"messages":[{"role":"user","content":"summarize"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.NewReader(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_compact\"}}\n\n" +
			"data: {\"type\":\"error\",\"message\":\"Your input exceeds the context window of this model\"}\n\n",
	)
	_, buffered, metrics, err := bufferCodexCompactionResponseObserved(stream, converted)
	if err == nil || !isCodexContextOverflow(err) {
		t.Fatalf("buffered compaction error = %v", err)
	}
	if len(buffered) != 0 {
		t.Fatalf("partial SSE escaped before retry: %q", buffered)
	}
	if metrics.events != 2 || metrics.terminalType != "error" || metrics.firstEventAt.IsZero() {
		t.Fatalf("stream metrics = %+v", metrics)
	}
}

func TestCodexCompactionRetriesSSEOverflowOnce(t *testing.T) {
	var calls atomic.Int32
	var requestSizes []int
	var requestTokens []int
	var sizesMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		tokens, _ := countCodexResponsesInputTokens(body)
		sizesMu.Lock()
		requestSizes = append(requestSizes, len(body))
		requestTokens = append(requestTokens, tokens)
		sizesMu.Unlock()
		call := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_compact\"}}\n\n")
		if call == 1 {
			_, _ = fmt.Fprint(writer, "data: {\"type\":\"error\",\"message\":\"Your input exceeds the context window of this model\"}\n\n")
			return
		}
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"compact summary\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()
	runtime, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "key", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	messages := make([]map[string]any, 0, 242)
	chunk := strings.Repeat("value := object[index] ?? fallback\n", 300)
	for index := 0; index < 240; index++ {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("history-%03d\n%s", index, chunk)})
	}
	messages = append(messages, map[string]any{
		"role": "user",
		"content": "Your task is to create a detailed summary of this conversation. " +
			"This summary will be placed at the start of a continuing session; newer messages will follow.",
	})
	payload, err := json.Marshal(map[string]any{
		"model": "gpt-test", "max_tokens": 4096, "stream": true,
		"system": "You are Claude Code.", "messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("compact summary")) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want exactly 2", calls.Load())
	}
	sizesMu.Lock()
	defer sizesMu.Unlock()
	if len(requestSizes) != 2 || requestSizes[1] >= requestSizes[0] {
		t.Fatalf("request sizes=%v, want a smaller single retry", requestSizes)
	}
	if len(requestTokens) != 2 || requestTokens[0] < codexCompactionSoftTargetTokens*3/4 ||
		requestTokens[0] > codexCompactionSoftTargetTokens+2_000 {
		t.Fatalf("preflight tokens=%v, want recent context close to %d", requestTokens, codexCompactionSoftTargetTokens)
	}
	if requestTokens[1] >= requestTokens[0] || requestTokens[1] > codexCompactionRetryTargetTokens+2_000 {
		t.Fatalf("retry tokens=%v, want a smaller retry near or below %d", requestTokens, codexCompactionRetryTargetTokens)
	}
}

func TestCodexCompactionTrimDropsOrphanedToolOutput(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call_output", "call_id": "old", "output": "orphan"},
		map[string]any{"type": "function_call", "call_id": "kept"},
		map[string]any{"type": "function_call_output", "call_id": "kept", "output": "paired"},
	}
	kept := codexDropOrphanedToolOutputs(input)
	encoded, _ := json.Marshal(kept)
	if strings.Contains(string(encoded), "orphan") || !strings.Contains(string(encoded), "paired") {
		t.Fatalf("normalized tool items = %s", encoded)
	}
}

func TestCodexContextOverflowClassification(t *testing.T) {
	if !isCodexContextOverflow(&codexResponsesUpstreamError{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"Your input exceeds the context window of this model"}}`,
	}) {
		t.Fatal("context overflow was not classified")
	}
	if isCodexContextOverflow(&codexResponsesUpstreamError{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"invalid tool schema"}}`,
	}) {
		t.Fatal("unrelated 400 was classified as context overflow")
	}
	if !isCodexContextOverflow(fmt.Errorf("Codex Responses stream error: input exceeds the context window")) {
		t.Fatal("SSE context overflow was not classified")
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
