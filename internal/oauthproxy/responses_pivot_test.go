package oauthproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestParseResponsesRequestCoversAllItemAndToolForms(t *testing.T) {
	// 1. String input shorthand + instructions.
	simple, err := parseResponsesRequest([]byte(`{
		"model": "gpt-test[1m]",
		"instructions": "Be brief.",
		"input": "Hello world"
	}`))
	if err != nil {
		t.Fatalf("parseResponsesRequest(string input) error: %v", err)
	}
	if simple.System != "Be brief." || len(simple.Turns) != 1 || simple.Turns[0].Role != "user" ||
		len(simple.Turns[0].Parts) != 1 || simple.Turns[0].Parts[0].Text != "Hello world" {
		t.Fatalf("unexpected simple parse: %+v", simple)
	}

	// 2. Complex input: developer message, custom tool, namespaced tool, client
	// tool_search with deferred tool loaded via tool_search_output, unavailable
	// historical tool call/result, and CCL-prefixed vs foreign reasoning.
	complexRaw := []byte(`{
		"model": "gpt-test",
		"instructions": "Base system.",
		"reasoning": {"effort": "high"},
		"tool_choice": {"type": "function", "namespace": "mcp_fs", "name": "read_file"},
		"tools": [
			{"type": "custom", "name": "apply_patch", "description": "Patch files", "format": {"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
			{"type": "namespace", "name": "mcp_fs", "tools": [
				{"type": "function", "name": "read_file", "description": "Read a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}
			]},
			{"type": "tool_search", "execution": "client"},
			{"type": "function", "name": "deferred_hidden", "defer_loading": true, "parameters": {"type": "object"}},
			{"type": "function", "name": "deferred_loaded", "defer_loading": true, "parameters": {"type": "object"}}
		],
		"input": [
			{"role": "developer", "content": "Extra developer instruction."},
			{"role": "user", "content": [{"type": "input_text", "text": "Start task"}]},
			{"type": "reasoning", "id": "rs_ccl_abc", "summary": [{"type": "summary_text", "text": "own reasoning"}], "encrypted_content": "sig-own"},
			{"type": "reasoning", "id": "rs_foreign_xyz", "summary": [{"type": "summary_text", "text": "foreign reasoning"}], "encrypted_content": "sig-foreign"},
			{"type": "tool_search_call", "call_id": "search_1", "arguments": {"query": "deferred"}},
			{"type": "tool_search_output", "call_id": "search_1", "tools": [
				{"type": "function", "name": "deferred_loaded", "description": "Loaded tool", "parameters": {"type": "object"}}
			]},
			{"type": "custom_tool_call", "call_id": "patch_1", "name": "apply_patch", "input": "*** Begin Patch"},
			{"type": "custom_tool_call_output", "call_id": "patch_1", "output": "patched"},
			{"type": "function_call", "call_id": "old_1", "name": "removed_tool", "arguments": "{\"k\":\"v\"}"},
			{"type": "function_call_output", "call_id": "old_1", "output": "old output"}
		]
	}`)
	parsed, err := parseResponsesRequest(complexRaw)
	if err != nil {
		t.Fatalf("parseResponsesRequest(complex) error: %v", err)
	}
	if !strings.Contains(parsed.System, "Base system.") || !strings.Contains(parsed.System, "Extra developer instruction.") {
		t.Fatalf("system instructions missing: %q", parsed.System)
	}
	if !strings.Contains(parsed.System, "Historical tool records are conversation context") {
		t.Fatalf("unavailable tool guidance missing from system: %q", parsed.System)
	}
	if parsed.ToolChoice.Mode != toolChoiceTool || parsed.ToolChoice.Name != toolWireName("mcp_fs", "read_file") {
		t.Fatalf("unexpected tool_choice: %+v", parsed.ToolChoice)
	}
	// Verify deferred_loaded is present and deferred_hidden was omitted.
	foundLoaded := false
	for _, tool := range parsed.Tools {
		if tool.Name == "deferred_hidden" {
			t.Fatalf("deferred_hidden should not be in active tools: %+v", parsed.Tools)
		}
		if tool.Name == "deferred_loaded" {
			foundLoaded = true
		}
	}
	if !foundLoaded {
		t.Fatalf("deferred_loaded was not loaded from tool_search_output: %+v", parsed.Tools)
	}
	// Verify CCL reasoning kept its signature while foreign reasoning was ignored.
	var sawOwnReasoning bool
	var sawDemotedCall bool
	for _, turn := range parsed.Turns {
		for _, part := range turn.Parts {
			if part.Kind == partReasoning {
				if part.Signature == "sig-foreign" {
					t.Fatal("foreign reasoning signature should not be retained")
				}
				if part.Signature == "sig-own" && part.Text == "own reasoning" {
					sawOwnReasoning = true
				}
			}
			if part.Kind == partText && strings.Contains(part.Text, "Historical tool call removed_tool") {
				sawDemotedCall = true
			}
		}
	}
	if !sawOwnReasoning || !sawDemotedCall {
		t.Fatalf("sawOwnReasoning=%v sawDemotedCall=%v turns=%+v", sawOwnReasoning, sawDemotedCall, parsed.Turns)
	}
}

func TestResponseAssemblerCustomToolsAndIncompleteAndFailedStates(t *testing.T) {
	req, err := parseResponsesRequest([]byte(`{
		"model": "gpt-test",
		"stream": true,
		"tools": [
			{"type": "custom", "name": "apply_patch"},
			{"type": "tool_search", "execution": "client"}
		],
		"input": "hi"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	adapter := req.adapter("gpt-test")
	recorder := httptest.NewRecorder()
	assembler := newResponseAssembler(&adapter, recorder)
	if err := assembler.addText("hello"); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("call_patch", "apply_patch", `{"input":"*** Begin Patch"}`); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("call_search", "tool_search", `{"query":"fs"}`); err != nil {
		t.Fatal(err)
	}
	assembler.stopReason = stopMaxTokens
	if err := assembler.finish(); err != nil {
		t.Fatal(err)
	}
	stream := recorder.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.custom_tool_call_input.delta",
		"event: response.custom_tool_call_input.done",
		`"type":"custom_tool_call"`,
		`"type":"tool_search_call"`,
		"event: response.incomplete",
		`"reason":"max_output_tokens"`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}

	// Verify fail() emits response.failed.
	failRec := httptest.NewRecorder()
	failAssembler := newResponseAssembler(&adapter, failRec)
	failAssembler.fail("rate_limit_error", "too many requests")
	if !strings.Contains(failRec.Body.String(), "event: response.failed") ||
		!strings.Contains(failRec.Body.String(), `"code":"rate_limit_error"`) {
		t.Fatalf("unexpected fail stream:\n%s", failRec.Body.String())
	}
}

// assertBothFrontDoors exercises /v1/responses and /v1/chat/completions in both
// streaming and non-streaming modes for both a text turn and a tool-call turn.
func assertBothFrontDoors(t *testing.T, handler http.Handler, apiKey, model string, setMode func(toolCall bool)) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	post := func(path string, payload any) string {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", path, resp.StatusCode, body)
		}
		return string(body)
	}

	responsesPayload := func(stream, toolCall bool) map[string]any {
		body := map[string]any{
			"model":  model,
			"stream": stream,
			"input":  []any{map[string]any{"role": "user", "content": "hello"}},
		}
		if toolCall {
			body["tools"] = []any{map[string]any{
				"type":        "function",
				"name":        "Read",
				"description": "Read a file",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			}}
		}
		return body
	}

	chatPayload := func(stream, toolCall bool) map[string]any {
		body := map[string]any{
			"model":    model,
			"stream":   stream,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
		if toolCall {
			body["tools"] = []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "Read",
					"description": "Read a file",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
				},
			}}
		}
		return body
	}

	// 1. Text path across all 4 combinations.
	setMode(false)
	respNonStreamText := post("/v1/responses", responsesPayload(false, false))
	if gjson.Get(respNonStreamText, "object").String() != "response" ||
		gjson.Get(respNonStreamText, "output.0.content.0.text").String() != "Hello world" {
		t.Fatalf("responses non-stream text = %s", respNonStreamText)
	}
	respStreamText := post("/v1/responses", responsesPayload(true, false))
	if !strings.Contains(respStreamText, "event: response.output_text.delta") ||
		!strings.Contains(respStreamText, "event: response.completed") {
		t.Fatalf("responses stream text = %s", respStreamText)
	}
	chatNonStreamText := post("/v1/chat/completions", chatPayload(false, false))
	if gjson.Get(chatNonStreamText, "object").String() != "chat.completion" ||
		gjson.Get(chatNonStreamText, "choices.0.message.content").String() != "Hello world" ||
		gjson.Get(chatNonStreamText, "choices.0.finish_reason").String() != "stop" {
		t.Fatalf("chat non-stream text = %s", chatNonStreamText)
	}
	chatStreamText := post("/v1/chat/completions", chatPayload(true, false))
	if !strings.Contains(chatStreamText, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(chatStreamText, `"content":"Hello world"`) ||
		!strings.Contains(chatStreamText, "data: [DONE]") {
		t.Fatalf("chat stream text = %s", chatStreamText)
	}

	// 2. Tool-call path across all 4 combinations.
	setMode(true)
	respNonStreamTool := post("/v1/responses", responsesPayload(false, true))
	if gjson.Get(respNonStreamTool, "output.0.type").String() != "function_call" ||
		gjson.Get(respNonStreamTool, "output.0.name").String() != "Read" ||
		gjson.Get(gjson.Get(respNonStreamTool, "output.0.arguments").String(), "path").String() != "/tmp/a" {
		t.Fatalf("responses non-stream tool = %s", respNonStreamTool)
	}
	respStreamTool := post("/v1/responses", responsesPayload(true, true))
	if !strings.Contains(respStreamTool, `"type":"function_call"`) ||
		!strings.Contains(respStreamTool, "event: response.completed") {
		t.Fatalf("responses stream tool = %s", respStreamTool)
	}
	chatNonStreamTool := post("/v1/chat/completions", chatPayload(false, true))
	if gjson.Get(chatNonStreamTool, "choices.0.finish_reason").String() != "tool_calls" ||
		gjson.Get(chatNonStreamTool, "choices.0.message.tool_calls.0.function.name").String() != "Read" ||
		gjson.Get(gjson.Get(chatNonStreamTool, "choices.0.message.tool_calls.0.function.arguments").String(), "path").String() != "/tmp/a" {
		t.Fatalf("chat non-stream tool = %s", chatNonStreamTool)
	}
	chatStreamTool := post("/v1/chat/completions", chatPayload(true, true))
	if !strings.Contains(chatStreamTool, `"tool_calls"`) ||
		!strings.Contains(chatStreamTool, `"finish_reason":"tool_calls"`) ||
		!strings.Contains(chatStreamTool, "data: [DONE]") {
		t.Fatalf("chat stream tool = %s", chatStreamTool)
	}
}

func TestAllRuntimesCoverResponsesAndChatCompletionsFrontDoors(t *testing.T) {
	t.Run("OpenAIChatRuntime", func(t *testing.T) {
		var toolMode bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			stream := gjson.GetBytes(raw, "stream").Bool()
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				if toolMode {
					_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"/tmp/a\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if toolMode {
				_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
			} else {
				_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
			}
		}))
		defer upstream.Close()
		service := newChatCompletionsService("local-key", upstream.URL+"/v1", []runtimeModelRoute{{Name: "gpt-test", Alias: "gpt-test"}}, "up", NewUsageTracker())
		assertBothFrontDoors(t, service.handler(), "local-key", "gpt-test", func(tc bool) { toolMode = tc })
	})

	t.Run("CodexResponsesRuntime", func(t *testing.T) {
		var toolMode bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if toolMode {
				_, _ = io.WriteString(w, strings.Join([]string{
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
					``,
					`event: response.output_item.added`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"","status":"in_progress"}}`,
					``,
					`event: response.function_call_arguments.delta`,
					`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":\"/tmp/a\"}"}`,
					``,
					`event: response.output_item.done`,
					`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"/tmp/a\"}","status":"completed"}}`,
					``,
					`event: response.completed`,
					`data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"/tmp/a\"}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
					``,
					``,
				}, "\n"))
				return
			}
			_, _ = io.WriteString(w, strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				``,
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello world"}`,
				``,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
				``,
				``,
			}, "\n"))
		}))
		defer upstream.Close()
		service := newXaiResponsesService("local-key", upstream.URL+"/v1", []runtimeModelRoute{{Name: "gpt-test", Alias: "gpt-test"}}, &codexStaticAuthorizer{token: "up"}, NewUsageTracker())
		assertBothFrontDoors(t, service.handler(), "local-key", "gpt-test", func(tc bool) { toolMode = tc })
	})

	t.Run("GeminiRuntime", func(t *testing.T) {
		var toolMode bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.RawQuery, "alt=sse") {
				w.Header().Set("Content-Type", "text/event-stream")
				if toolMode {
					_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"id\":\"call_1\",\"name\":\"Read\",\"args\":{\"path\":\"/tmp/a\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2}}}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello world\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2}}}\n\n")
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if toolMode {
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"Read","args":{"path":"/tmp/a"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`)
			} else {
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`)
			}
		}))
		defer upstream.Close()
		prevDaily, prevProd := antigravityBaseURLDaily, antigravityBaseURLProd
		antigravityBaseURLDaily, antigravityBaseURLProd = upstream.URL, upstream.URL
		t.Cleanup(func() { antigravityBaseURLDaily, antigravityBaseURLProd = prevDaily, prevProd })
		service := &geminiService{
			apiKey: "local-key", authorizer: &chatStaticAuthorizer{token: "tok"},
			models: []string{"gemini-2.5-pro"}, client: upstream.Client(),
			projectID: "proj", baseURL: upstream.URL,
		}
		assertBothFrontDoors(t, service.handler(), "local-key", "gemini-2.5-pro", func(tc bool) { toolMode = tc })
	})

	t.Run("KiroRuntime", func(t *testing.T) {
		var toolMode bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			if toolMode {
				_, _ = w.Write(encodeKiroTestFrame(t, "toolUseEvent", map[string]any{"name": "Read", "toolUseId": "call_1", "input": `{"path":"/tmp/a"}`, "stop": true}))
				return
			}
			_, _ = w.Write(encodeKiroTestFrame(t, "assistantResponseEvent", map[string]any{"content": "Hello world"}))
		}))
		defer upstream.Close()
		authDir := t.TempDir()
		rawCred, _ := json.Marshal(map[string]any{
			"type": "kiro", "access_token": "tok", "refresh_token": "ref",
			"profile_arn": kiroBuilderProfileARN, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
		_ = os.WriteFile(filepath.Join(authDir, "kiro-test.json"), rawCred, 0o600)
		service := &kiroService{
			apiKey: "local-key", models: []string{"claude-sonnet-4-6"},
			pool: newKiroCredentialPool(authDir, "kiro-test.json"), client: upstream.Client(),
			upstreamURL: func(*kiroCredential) string { return upstream.URL },
		}
		assertBothFrontDoors(t, service.handler(), "local-key", "claude-sonnet-4-6", func(tc bool) { toolMode = tc })
	})

	t.Run("CursorRuntime", func(t *testing.T) {
		var toolMode bool
		newCursorStubUpstream(t, func(w http.ResponseWriter, r *http.Request, frames <-chan cursorStubFrame) {
			<-frames
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			if toolMode {
				writeCursorFrame(w, cursorTestExecMCP(1, "exec-1", "Read", "call_1", map[string]string{"path": "/tmp/a"}))
				<-r.Context().Done()
				return
			}
			writeCursorFrame(w, cursorTestInteraction(cursorIUTextDelta, "Hello world"))
			writeCursorFrame(w, cursorTestTurnEnd())
		})
		service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
		defer service.closeParked()
		assertBothFrontDoors(t, service.handler(), service.apiKey, "composer-2", func(tc bool) {
			service.closeParked()
			toolMode = tc
		})
	})

	t.Run("QoderRuntime", func(t *testing.T) {
		var toolMode bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if toolMode {
				writeQoderTestSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "Read", "arguments": `{"path":"/tmp/a"}`}},
				}}}}})
				writeQoderTestSSE(w, map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "delta": map[string]any{}}}})
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			writeQoderTestSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "Hello world"}}}})
			writeQoderTestSSE(w, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "delta": map[string]any{}}}})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()
		prevAPI := qoderAPIBaseURL
		qoderAPIBaseURL = upstream.URL
		t.Cleanup(func() { qoderAPIBaseURL = prevAPI })
		authDir := t.TempDir()
		rawCred, _ := json.Marshal(map[string]any{
			"type": "qoder", "access_token": "tok", "refresh_token": "ref",
			"user_id": "u1", "machine_id": "m1",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
		_ = os.WriteFile(filepath.Join(authDir, "qoder-test.json"), rawCred, 0o600)
		service := &qoderService{
			apiKey: "local-key",
			models: []qoderModel{{ID: "auto"}},
			pool:   newQoderCredentialPool(authDir, "qoder-test.json"),
			client: upstream.Client(),
		}
		assertBothFrontDoors(t, service.handler(), "local-key", "auto", func(tc bool) { toolMode = tc })
	})
}
