package oauthproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/tidwall/gjson"
)

// minimal 1x1 PNG bytes for magic-byte sniffing & screenshot plumbing tests.
var onePixelPNGBase64 = base64.StdEncoding.EncodeToString([]byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
})

func TestStatefulPreviousResponseIDConvertedAndPassthrough(t *testing.T) {
	t.Run("converted_chat_multi_turn", func(t *testing.T) {
		var turn atomic.Int32
		var capturedBodies []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			capturedBodies = append(capturedBodies, string(raw))
			isStream := gjson.GetBytes(raw, "stream").Bool()
			switch turn.Add(1) {
			case 1:
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"checking tools\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_ls\",\"type\":\"function\",\"function\":{\"name\":\"list_dir\",\"arguments\":\"{\\\"path\\\":\\\".\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			case 2:
				if !isStream {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"cmpl_2","choices":[{"message":{"role":"assistant","content":"Found README.md"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Found README.md\"},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			default:
				if !isStream {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"cmpl_3","choices":[{"message":{"role":"assistant","content":"Turn 3 complete"},"finish_reason":"stop"}],"usage":{"prompt_tokens":14,"completion_tokens":4}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Turn 3 complete\"},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}
		}))
		defer upstream.Close()

		rt, err := StartOpenAIChatAPI(t.Context(), upstream.URL, "up-key", "test-model")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		post := func(payload map[string]any) (int, []byte) {
			t.Helper()
			encoded, _ := json.Marshal(payload)
			req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", bytes.NewReader(encoded))
			req.Header.Set("Authorization", "Bearer "+rt.APIKey())
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, body
		}

		// Unknown previous_response_id returns explicit 400.
		status, errBody := post(map[string]any{
			"model":                "test-model",
			"previous_response_id": "resp_does_not_exist",
			"input":                "hello",
		})
		if status != http.StatusBadRequest || gjson.GetBytes(errBody, "error.code").String() != "previous_response_not_found" {
			t.Fatalf("expected 400 previous_response_not_found, got status=%d body=%s", status, errBody)
		}

		// Turn 1 (streaming): emits response.completed with tool call.
		status, sse1 := post(map[string]any{
			"model":  "test-model",
			"stream": true,
			"input":  "list files",
			"tools": []any{
				map[string]any{"type": "function", "name": "list_dir", "parameters": map[string]any{"type": "object"}},
			},
		})
		if status != http.StatusOK {
			t.Fatalf("turn 1 status=%d body=%s", status, sse1)
		}
		var respID1 string
		for _, frame := range strings.Split(string(sse1), "\n\n") {
			if strings.Contains(frame, `"response.completed"`) {
				for _, line := range strings.Split(frame, "\n") {
					if after, ok := strings.CutPrefix(line, "data: "); ok {
						respID1 = gjson.Get(after, "response.id").String()
					}
				}
			}
		}
		if respID1 == "" {
			t.Fatalf("missing response.id in turn 1 SSE:\n%s", sse1)
		}

		// Turn 2 (non-streaming): sends ONLY previous_response_id + function_call_output.
		status, json2 := post(map[string]any{
			"model":                "test-model",
			"stream":               false,
			"previous_response_id": respID1,
			"input": []any{
				map[string]any{"type": "function_call_output", "call_id": "call_ls", "output": "README.md"},
			},
			"tools": []any{
				map[string]any{"type": "function", "name": "list_dir", "parameters": map[string]any{"type": "object"}},
			},
		})
		if status != http.StatusOK {
			t.Fatalf("turn 2 status=%d body=%s", status, json2)
		}
		respID2 := gjson.GetBytes(json2, "id").String()
		if respID2 == "" || !strings.Contains(string(json2), "Found README.md") {
			t.Fatalf("turn 2 body=%s", json2)
		}
		if len(capturedBodies) != 2 || !strings.Contains(capturedBodies[1], "list files") || !strings.Contains(capturedBodies[1], `"tool_call_id":"call_ls"`) {
			t.Fatalf("turn 2 upstream body missing expanded history: %v", capturedBodies)
		}

		// Turn 3 chains off respID2.
		status, json3 := post(map[string]any{
			"model":                "test-model",
			"stream":               false,
			"previous_response_id": respID2,
			"input":                "summarize it",
		})
		if status != http.StatusOK || !strings.Contains(string(json3), "Turn 3 complete") {
			t.Fatalf("turn 3 status=%d body=%s", status, json3)
		}
		if len(capturedBodies) != 3 || !strings.Contains(capturedBodies[2], "Found README.md") || !strings.Contains(capturedBodies[2], "summarize it") {
			t.Fatalf("turn 3 upstream body missing chained history: %s", capturedBodies[2])
		}
	})

	t.Run("passthrough_store_false_strips_unencrypted_reasoning", func(t *testing.T) {
		var turn atomic.Int32
		var turn2Upstream []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			if turn.Add(1) == 1 {
				completed, _ := json.Marshal(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     "resp_xai_1",
						"object": "response",
						"status": "completed",
						"output": []any{
							// Unencrypted reasoning (e.g. xAI summary-only) must be stripped on store=false replay.
							map[string]any{"id": "rs_unenc", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "thinking"}}},
							// Native encrypted reasoning must survive untouched.
							map[string]any{"id": "rs_enc", "type": "reasoning", "summary": []any{}, "encrypted_content": "enc_blob"},
							map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "ping", "arguments": "{}"},
						},
					},
				})
				fmt.Fprintf(w, "data: %s\n\n", completed)
				return
			}
			turn2Upstream = raw
			completed, _ := json.Marshal(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     "resp_xai_2",
					"object": "response",
					"status": "completed",
					"output": []any{
						map[string]any{"id": "msg_2", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "pong"}}},
					},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", completed)
		}))
		defer upstream.Close()

		rt, err := startCodexResponsesRuntime(t.Context(), upstream.URL, "grok-4.6", &codexStaticAuthorizer{token: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		doPost := func(body string) (int, []byte) {
			req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rt.APIKey())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, raw
		}

		if status, body := doPost(`{"model":"grok-4.6","input":"run ping"}`); status != http.StatusOK {
			t.Fatalf("turn 1 status=%d body=%s", status, body)
		}
		status, body := doPost(`{"model":"grok-4.6","previous_response_id":"resp_xai_1","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
		if status != http.StatusOK || !strings.Contains(string(body), "pong") {
			t.Fatalf("turn 2 status=%d body=%s", status, body)
		}
		if gjson.GetBytes(turn2Upstream, "store").Bool() != false {
			t.Fatalf("store=false violated: %s", turn2Upstream)
		}
		if gjson.GetBytes(turn2Upstream, "previous_response_id").Exists() {
			t.Fatalf("previous_response_id leaked upstream: %s", turn2Upstream)
		}
		if strings.Contains(string(turn2Upstream), "rs_unenc") {
			t.Fatalf("unencrypted reasoning leaked to store=false upstream: %s", turn2Upstream)
		}
		if !strings.Contains(string(turn2Upstream), "rs_enc") || !strings.Contains(string(turn2Upstream), "call_1") {
			t.Fatalf("encrypted reasoning or tool call missing on turn 2: %s", turn2Upstream)
		}
	})
}

func TestBuiltinClientToolsAndDemotion(t *testing.T) {
	// 1. Active declarations: computer_use_preview, shell, local_shell, apply_patch, tool_search (omitted parameters).
	reqRaw, _ := json.Marshal(map[string]any{
		"model": "gpt-test",
		"tools": []any{
			map[string]any{"type": "computer_use_preview", "display_width": 1280, "display_height": 800, "environment": "mac"},
			map[string]any{"type": "shell"},
			map[string]any{"type": "local_shell"},
			map[string]any{"type": "apply_patch"},
			map[string]any{"type": "tool_search", "execution": "client"},
		},
		"input": []any{
			map[string]any{"type": "mcp_list_tools", "id": "mcpl_1", "server_label": "local", "tools": []any{}},
			map[string]any{"type": "message", "role": "user", "content": "click start"},
			map[string]any{"type": "computer_call", "call_id": "cu_1", "action": map[string]any{"type": "click", "x": 10, "y": 20}},
			map[string]any{
				"type":    "computer_call_output",
				"call_id": "cu_1",
				"output":  map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;charset=utf-8;base64," + onePixelPNGBase64},
			},
			map[string]any{"type": "shell_call", "call_id": "sh_1", "action": map[string]any{"commands": []any{"ls"}}},
			map[string]any{
				"type":    "shell_call_output",
				"call_id": "sh_1",
				"output":  []any{map[string]any{"stdout": "main.go\n", "stderr": "", "outcome": map[string]any{"type": "exit", "exit_code": 0}}},
			},
		},
	})

	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatalf("parseResponsesRequest failed: %v", err)
	}
	if len(parsed.Tools) != 5 {
		t.Fatalf("expected 5 adapted client tools, got %d", len(parsed.Tools))
	}
	// Verify assembler re-emits computer_call, shell_call, local_shell_call, apply_patch_call, tool_search_call.
	adapter := parsed.adapter("gpt-test")
	assembler := newResponseAssembler(&adapter, nil)
	if err := assembler.addToolUse("c1", "computer", `{"type":"click","x":42,"y":84}`); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("s1", "shell", `{"commands":["pwd"]}`); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("ls1", "local_shell", `{"command":["ls","-la"]}`); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("ap1", "apply_patch", `{"type":"update_file","path":"a.go","diff":"+ok"}`); err != nil {
		t.Fatal(err)
	}
	if err := assembler.addToolUse("ts1", "tool_search", `{"query":"browser"}`); err != nil {
		t.Fatal(err)
	}
	items := assembler.outputItems()
	wantTypes := []string{"computer_call", "shell_call", "local_shell_call", "apply_patch_call", "tool_search_call"}
	for i, want := range wantTypes {
		if got := stringValue(items[i]["type"]); got != want {
			t.Fatalf("item[%d].type = %q, want %q (item=%+v)", i, got, want, items[i])
		}
	}

	// 2. Cross-model demotion when tools are NOT declared: preserves screenshot partImage without orphan tool_result.
	demotedRaw, _ := json.Marshal(map[string]any{
		"model": "text-or-other-model",
		"input": []any{
			map[string]any{"type": "computer_call", "call_id": "cu_old", "action": map[string]any{"type": "screenshot"}},
			map[string]any{
				"type":    "computer_call_output",
				"call_id": "cu_old",
				"output":  map[string]any{"type": "computer_screenshot", "image_url": "data:;base64," + onePixelPNGBase64},
			},
		},
	})
	demoted, err := parseResponsesRequest(demotedRaw)
	if err != nil {
		t.Fatalf("demoted parse failed: %v", err)
	}
	foundImage := false
	for _, turn := range demoted.Turns {
		for _, part := range turn.Parts {
			if part.Kind == partToolCall || part.Kind == partToolResult {
				t.Fatalf("undeclared computer_call should demote cleanly, got %+v", part)
			}
			if part.Kind == partImage && part.MediaType == "image/png" {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("MIME-less PNG screenshot lost during computer_call_output demotion: %+v", demoted.Turns)
	}
}

func TestChatFrontCustomToolCallStreamingAndNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_exec\",\"type\":\"function\",\"function\":{\"name\":\"exec\",\"arguments\":\"{\\\"input\\\":\\\"echo hi\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	// Verify chatFrontCompletion and chatFrontWriter render custom_tool_call as function tool_calls.
	respObj := map[string]any{
		"id":     "resp_custom",
		"object": "response",
		"status": "completed",
		"output": []any{
			map[string]any{
				"id":      "fc_call_exec",
				"type":    "custom_tool_call",
				"call_id": "call_exec",
				"name":    "exec",
				"input":   "echo hi",
				"status":  "completed",
			},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	completion := chatFrontCompletion(respObj, "chatcmpl_1", 123, "gpt-test")
	rawComp, _ := json.Marshal(completion)
	if gjson.GetBytes(rawComp, "choices.0.finish_reason").String() != "tool_calls" ||
		gjson.GetBytes(rawComp, "choices.0.message.tool_calls.0.function.name").String() != "exec" ||
		gjson.Get(gjson.GetBytes(rawComp, "choices.0.message.tool_calls.0.function.arguments").String(), "input").String() != "echo hi" {
		t.Fatalf("chatFrontCompletion lost custom_tool_call: %s", rawComp)
	}

	recorder := httptest.NewRecorder()
	writer := newChatFrontWriter(recorder, "gpt-test", false)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	for _, frame := range []string{
		`data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"custom_tool_call","call_id":"call_exec","name":"exec","input":""}}` + "\n\n",
		`data: {"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"fc_1","input":"echo hi"}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}` + "\n\n",
	} {
		_, _ = writer.Write([]byte(frame))
	}
	writer.finish()
	streamOut := recorder.Body.String()
	if !strings.Contains(streamOut, `"name":"exec"`) || !strings.Contains(streamOut, `\"input\":\"echo hi\"`) || !strings.Contains(streamOut, `"finish_reason":"tool_calls"`) {
		t.Fatalf("chatFrontWriter streaming lost custom_tool_call:\n%s", streamOut)
	}
}

func TestModelsEndpointAndVisionModalitiesContract(t *testing.T) {
	// 1. serveModelListWithDetails populates api_backend: "responses", context_window, max_output_tokens, input_modalities.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	serveModelListWithDetails(rec, req, "test-key", []string{"vision-pro", "plain-unknown"}, map[string]protocol.ModelInfo{
		"vision-pro": {
			ID:              "vision-pro",
			DisplayName:     "Vision Pro",
			ContextWindow:   500_000,
			MaxOutputTokens: 64_000,
			InputModalities: []string{"text", "image"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if gjson.GetBytes(body, "data.0.api_backend").String() != "responses" ||
		gjson.GetBytes(body, "data.0.context_window").Int() != 500_000 ||
		gjson.GetBytes(body, "data.0.max_output_tokens").Int() != 64_000 ||
		gjson.GetBytes(body, "data.0.input_modalities.1").String() != "image" {
		t.Fatalf("vision-pro /v1/models entry = %s", body)
	}
	if gjson.GetBytes(body, "data.1.api_backend").String() != "responses" ||
		gjson.GetBytes(body, "data.1.input_modalities").Exists() {
		t.Fatalf("plain-unknown invented input_modalities: %s", body)
	}

	// 2. AutoClaw catalog preserves input modalities from contract without inventing vision on text-only models.
	acDetails := autoClawModelDetailsWithCatalog(
		[]runtimeModelRoute{{Name: "vision-m", Alias: "vision-alias"}, {Name: "text-m", Alias: "text-alias"}},
		[]autoclawModelDefinition{
			{id: "vision-m", name: "Vision M", context: 200_000, maxOutput: 16_000, input: []string{"text", "image"}},
			{id: "text-m", name: "Text M", context: 128_000, maxOutput: 8_000, input: []string{"text"}},
		},
	)
	if !slices.Equal(acDetails["vision-alias"].InputModalities, []string{"text", "image"}) {
		t.Fatalf("AutoClaw vision-alias modalities = %+v", acDetails["vision-alias"])
	}
	if !slices.Equal(acDetails["text-alias"].InputModalities, []string{"text"}) {
		t.Fatalf("AutoClaw text-alias modalities = %+v", acDetails["text-alias"])
	}

	// 3. xAI catalog parses modalities & max_output_tokens while leaving unknown non-grok4 text-only.
	xaiModels, err := parseXaiModels([]byte(`{"data":[
		{"id":"grok-4.20","name":"Grok 4.20","api_backend":"responses","context_window":2000000,"max_output_tokens":65536},
		{"id":"custom-experimental","name":"Custom Exp","api_backend":"responses","context_window":128000}
	]}`))
	if err != nil || len(xaiModels) != 2 {
		t.Fatalf("parseXaiModels: models=%+v err=%v", xaiModels, err)
	}
	if !slices.Equal(xaiModels[0].InputModalities, []string{"text", "image"}) || xaiModels[0].MaxOutputTokens != 65536 {
		t.Fatalf("grok-4.20 = %+v", xaiModels[0])
	}
	if len(xaiModels[1].InputModalities) != 0 {
		t.Fatalf("invented vision for unknown xAI model: %+v", xaiModels[1])
	}

	// 4. Kiro, Kimi, and Cursor advertise ["text", "image"] for known multimodal models and NOT for unknown models.
	kiroDetails := kiroInitialModelDetails("claude-sonnet-4-6,mystery-model", []string{"claude-sonnet-4-6", "mystery-model"})
	if !slices.Equal(kiroDetails["claude-sonnet-4-6"].InputModalities, []string{"text", "image"}) {
		t.Fatalf("kiro claude-sonnet-4-6 = %+v", kiroDetails["claude-sonnet-4-6"])
	}
	if _, exists := kiroDetails["mystery-model"]; exists && slices.Contains(kiroDetails["mystery-model"].InputModalities, "image") {
		t.Fatalf("kiro invented vision for mystery-model: %+v", kiroDetails["mystery-model"])
	}

	kimiDetails := kimiModelDetails([]runtimeModelRoute{
		{Name: "kimi-for-coding", Alias: "kimi-for-coding"},
		{Name: "unknown-custom", Alias: "unknown-custom"},
	})
	if !slices.Equal(kimiDetails["kimi-for-coding"].InputModalities, []string{"text", "image"}) {
		t.Fatalf("kimi-for-coding = %+v", kimiDetails["kimi-for-coding"])
	}
	if _, exists := kimiDetails["unknown-custom"]; exists && slices.Contains(kimiDetails["unknown-custom"].InputModalities, "image") {
		t.Fatalf("kimi invented vision for unknown-custom: %+v", kimiDetails["unknown-custom"])
	}

	cursorRoutes := map[string]string{"claude-4.5-sonnet": "claude-4.5-sonnet", "cursor-small": "cursor-small"}
	curDetails := cursorModelDetails([]string{"claude-4.5-sonnet", "cursor-small"}, cursorRoutes)
	if !slices.Equal(curDetails["claude-4.5-sonnet"].InputModalities, []string{"text", "image"}) {
		t.Fatalf("cursor claude-4.5-sonnet = %+v", curDetails["claude-4.5-sonnet"])
	}
	if slices.Contains(curDetails["cursor-small"].InputModalities, "image") {
		t.Fatalf("cursor-small should stay text-only: %+v", curDetails["cursor-small"])
	}
}

func TestConverterFixesCursorKiroGeminiQoder(t *testing.T) {
	t.Run("cursor_preserves_tool_name_map_and_restarts_run_on_screenshot", func(t *testing.T) {
		rawNamespaceReq, _ := json.Marshal(map[string]any{
			"model": "claude-4.5-sonnet",
			"tools": []any{
				map[string]any{
					"type": "namespace",
					"name": "computer",
					"tools": []any{
						map[string]any{"type": "function", "name": "screenshot", "parameters": map[string]any{"type": "object"}},
					},
				},
			},
			"input": "take a screenshot",
		})
		converted, err := convertResponsesToCursor(rawNamespaceReq)
		if err != nil {
			t.Fatal(err)
		}
		if len(converted.tools) != 1 {
			t.Fatalf("expected 1 hashed MCP tool, got %+v", converted.tools)
		}
		curAssembler := newResponseAssembler(&converted.adapterRequest, nil)
		if err := curAssembler.addToolUse("call_ns_1", converted.tools[0].Name, `{}`); err != nil {
			t.Fatalf("namespaced Cursor tool failed to reassemble: %v", err)
		}
		nsOut := curAssembler.outputItems()
		if len(nsOut) != 1 || nsOut[0]["name"] != "screenshot" || nsOut[0]["namespace"] != "computer" {
			t.Fatalf("Cursor namespaced tool not restored: %+v", nsOut)
		}

		// tool_choice: "none" omits tools.
		rawNone, _ := json.Marshal(map[string]any{
			"model":       "claude-4.5-sonnet",
			"tool_choice": "none",
			"tools":       []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
			"input":       "hi",
		})
		convertedNone, err := convertResponsesToCursor(rawNone)
		if err != nil || len(convertedNone.tools) != 0 {
			t.Fatalf("tool_choice=none leaked tools: %+v err=%v", convertedNone, err)
		}

		// Parked stream + screenshot in function_call_output starts a fresh Run carrying SelectedImages.
		var runCount atomic.Int32
		var secondRunBytes []byte
		newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
			n := runCount.Add(1)
			firstFrame := <-frames
			writer.Header().Set("Content-Type", "application/connect+proto")
			writer.WriteHeader(http.StatusOK)
			if n == 1 {
				// Emit TWO parallel exec_mcp events in a single Connect frame!
				var batch []byte
				batch = append(batch, cursorTestExecMCP(1, "exec-1", "Screen", "call_img_1", map[string]string{})...)
				batch = append(batch, cursorTestExecMCP(2, "exec-2", "Status", "call_img_2", map[string]string{})...)
				writeCursorFrame(writer, batch)
				<-request.Context().Done()
				return
			}
			secondRunBytes = firstFrame.Body
			writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "saw screenshot"))
			writeCursorFrame(writer, cursorTestTurnEnd())
		})
		service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
		defer service.closeParked()

		turn1Body, _ := json.Marshal(map[string]any{
			"model":  "claude-4.5-sonnet",
			"stream": true,
			"tools": []any{
				map[string]any{"type": "function", "name": "Screen", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "function", "name": "Status", "parameters": map[string]any{"type": "object"}},
			},
			"input": "capture",
		})
		req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(turn1Body))
		req1.Header.Set("Authorization", "Bearer "+service.apiKey)
		status1, sse1 := serveCursorTurn(t, service, req1)
		if status1 != http.StatusOK || !strings.Contains(sse1, "call_img_1") || !strings.Contains(sse1, "call_img_2") {
			t.Fatalf("parallel exec_mcp events not both emitted: status=%d sse=\n%s", status1, sse1)
		}

		// Turn 2 returns tool results WITH a screenshot -> must close parked stream and open Run #2!
		turn2Body, _ := json.Marshal(map[string]any{
			"model":  "claude-4.5-sonnet",
			"stream": true,
			"tools": []any{
				map[string]any{"type": "function", "name": "Screen", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "function", "name": "Status", "parameters": map[string]any{"type": "object"}},
			},
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "capture"},
				map[string]any{"type": "function_call", "call_id": "call_img_1", "name": "Screen", "arguments": "{}"},
				map[string]any{"type": "function_call", "call_id": "call_img_2", "name": "Status", "arguments": "{}"},
				map[string]any{
					"type":    "function_call_output",
					"call_id": "call_img_1",
					"output": []any{
						map[string]any{"type": "input_text", "text": "captured window"},
						map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64},
					},
				},
				map[string]any{"type": "function_call_output", "call_id": "call_img_2", "output": "ok"},
			},
		})
		req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(turn2Body))
		req2.Header.Set("Authorization", "Bearer "+service.apiKey)
		status2, sse2 := serveCursorTurn(t, service, req2)
		if status2 != http.StatusOK || !strings.Contains(sse2, "saw screenshot") {
			t.Fatalf("turn 2 screenshot run failed: status=%d sse=\n%s", status2, sse2)
		}
		if runCount.Load() != 2 || len(secondRunBytes) == 0 || !bytes.Contains(secondRunBytes, []byte("PNG")) {
			t.Fatalf("expected fresh Run #2 carrying raw PNG bytes, got runCount=%d len=%d", runCount.Load(), len(secondRunBytes))
		}
	})

	t.Run("kiro_schema_tool_choice_and_image_sniffing", func(t *testing.T) {
		img := kiroImageFromPart(conversationPart{Kind: partImage, MediaType: "", Data: onePixelPNGBase64})
		if img == nil || img.(map[string]any)["format"] != "png" {
			t.Fatalf("kiroImageFromPart failed magic-byte PNG sniff: %#v", img)
		}
		paramImg := kiroImageFromPart(conversationPart{Kind: partImage, MediaType: "IMAGE/JPG; charset=binary", Data: onePixelPNGBase64})
		if paramImg == nil || paramImg.(map[string]any)["format"] != "jpeg" {
			t.Fatalf("kiroImageFromPart failed parameterized MIME: %#v", paramImg)
		}
		content := parseKiroContent(conversationTurn{
			Role: "user",
			Parts: []conversationPart{{
				Kind:   partToolResult,
				CallID: "call_1",
				Output: []conversationPart{
					{Kind: partText, Text: "coords: 10,20"},
					{Kind: partImage, MediaType: "image/png", Data: onePixelPNGBase64},
				},
			}},
		}, map[string]string{})
		gotText := content.toolResults[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if gotText != "coords: 10,20\n[image attached]" {
			t.Fatalf("expected [image attached] marker appended to non-empty tool text, got %q", gotText)
		}

		schema := normalizeKiroSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"choice": map[string]any{
					"anyOf": []any{
						map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "type": "integer", "maximum": float64(9_007_199_254_740_991)},
						map[string]any{"type": "null"},
					},
				},
			},
		}, true).(map[string]any)
		variant := schema["properties"].(map[string]any)["choice"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
		if _, hasSchema := variant["$schema"]; hasSchema {
			t.Fatalf("nested $schema survived normalizeKiroSchema: %#v", variant)
		}
		if _, hasMax := variant["maximum"]; hasMax {
			t.Fatalf("nested >32-bit maximum survived normalizeKiroSchema: %#v", variant)
		}

		rawNone, _ := json.Marshal(map[string]any{
			"model":       "claude-sonnet-4-6",
			"tool_choice": "none",
			"tools":       []any{map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}}},
			"input":       "hello",
		})
		kiroConv, err := convertResponsesToKiro(rawNone)
		if err != nil {
			t.Fatal(err)
		}
		kiroJSON, _ := json.Marshal(kiroConv.body)
		if gjson.GetBytes(kiroJSON, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools").Exists() {
			t.Fatalf("tool_choice=none leaked tools into Kiro payload: %s", kiroJSON)
		}
	})

	t.Run("gemini_sniff_and_qoder_tool_turn_and_stream_deltas", func(t *testing.T) {
		gemPart, ok := geminiImagePart(conversationPart{Kind: partImage, MediaType: "", Data: onePixelPNGBase64})
		if !ok || gjson.Get(gemPart, "inline_data.mime_type").String() != "image/png" {
			t.Fatalf("geminiImagePart failed magic-byte PNG sniff: ok=%t part=%s", ok, gemPart)
		}

		// Qoder: interleaved text + tool_result keeps role="tool" first.
		msgs, _ := qoderTransformUserTurn(conversationTurn{
			Role: "user",
			Parts: []conversationPart{
				{Kind: partText, Text: "system reminder"},
				{Kind: partToolResult, CallID: "call_q1", Output: []conversationPart{{Kind: partText, Text: "tool-ok"}}},
			},
		})
		if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "tool" || msgs[1].(map[string]any)["role"] != "user" {
			t.Fatalf("interleaved text broke Qoder tool-call adjacency: %+v", msgs)
		}

		// Qoder: tool_choice=none omits tools; temperature/top_p/stop forwarded.
		qRaw, _ := json.Marshal(map[string]any{
			"model":       "qoder-m",
			"temperature": 0.2,
			"top_p":       0.9,
			"stop":        []string{"END"},
			"tool_choice": "none",
			"tools":       []any{map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{"type": "object"}}},
			"input":       "hello",
		})
		qConv, err := convertResponsesToQoder(qRaw, []qoderModel{{ID: "qoder-m", Name: "Qoder M", MaxTokens: 8192}})
		if err != nil {
			t.Fatal(err)
		}
		qJSON, _ := json.Marshal(qConv.body)
		if gjson.GetBytes(qJSON, "tools.#").Int() != 0 ||
			gjson.GetBytes(qJSON, "parameters.temperature").Float() != 0.2 ||
			gjson.GetBytes(qJSON, "parameters.top_p").Float() != 0.9 ||
			gjson.GetBytes(qJSON, "parameters.stop.0").String() != "END" {
			t.Fatalf("Qoder parameters/tool_choice conversion mismatch: %s", qJSON)
		}

		// Qoder stream merges fragmented tool names ("read_" + "file") via chatToolNameDelta.
		sse := strings.Join([]string{
			`data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_frag","type":"function","function":{"name":"read_","arguments":"{\"p\":"}}]}}]}}`,
			`data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"\"a\"}"}}]},"finish_reason":"tool_calls"}]}}`,
			`data: [DONE]`,
			"",
		}, "\n\n")
		reqParsed, err := parseResponsesRequest([]byte(`{"model":"qoder-m","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		adapter := reqParsed.adapter("qoder-m")
		assembler := newResponseAssembler(&adapter, nil)
		if _, err := processQoderEventStream(t.Context(), strings.NewReader(sse), assembler); err != nil {
			t.Fatalf("processQoderEventStream fragmented name failed: %v", err)
		}
		out := assembler.outputItems()
		if len(out) != 1 || out[0]["name"] != "read_file" {
			t.Fatalf("fragmented Qoder tool name = %+v, want read_file", out)
		}
	})
}

func TestPhase2MixedCaseModelDetailsLookup(t *testing.T) {
	cat := &geminiCatalog{
		Models: []protocol.ModelInfo{
			{
				ID:              "Gemini-4-Argon",
				DisplayName:     "Gemini 4 Argon",
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
				InputModalities: []string{"text", "image"},
			},
		},
	}
	ids, _, details := cat.publicModels()
	if _, hasExact := details["Gemini-4-Argon"]; !hasExact {
		t.Fatalf("expected mixed-case key in geminiCatalog.publicModels() details: %+v", details)
	}

	modelsToServe := append(append([]string(nil), ids...), "gemini-4-argon")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	serveModelListWithDetails(rec, req, "test-key", modelsToServe, details)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d: %s", rec.Code, rec.Body.String())
	}

	raw := rec.Body.Bytes()
	if gjson.GetBytes(raw, "data.#").Int() != 2 {
		t.Fatalf("expected 2 model entries, got %s", raw)
	}
	for i := range 2 {
		prefix := fmt.Sprintf("data.%d", i)
		if got := gjson.GetBytes(raw, prefix+".api_backend").String(); got != "responses" {
			t.Fatalf("entry %d api_backend = %q, want responses (%s)", i, got, raw)
		}
		if got := gjson.GetBytes(raw, prefix+".display_name").String(); got != "Gemini 4 Argon" {
			t.Fatalf("entry %d display_name = %q, want Gemini 4 Argon (%s)", i, got, raw)
		}
		if got := gjson.GetBytes(raw, prefix+".context_window").Int(); got != 1_048_576 {
			t.Fatalf("entry %d context_window = %d, want 1048576 (%s)", i, got, raw)
		}
		if got := gjson.GetBytes(raw, prefix+".max_input_tokens").Int(); got != 1_048_576 {
			t.Fatalf("entry %d max_input_tokens = %d, want 1048576 (%s)", i, got, raw)
		}
		if got := gjson.GetBytes(raw, prefix+".max_output_tokens").Int(); got != 65_536 {
			t.Fatalf("entry %d max_output_tokens = %d, want 65536 (%s)", i, got, raw)
		}
		if got := gjson.GetBytes(raw, prefix+".input_modalities").Raw; got != `["text","image"]` {
			t.Fatalf("entry %d input_modalities = %s, want [\"text\",\"image\"] (%s)", i, got, raw)
		}
	}
}

func TestPhase2ModelsReasoningEffortAcrossRuntimes(t *testing.T) {
	extractEffortValues := func(entry gjson.Result) []string {
		var out []string
		for _, item := range entry.Get("reasoning_efforts").Array() {
			out = append(out, item.Get("value").String())
		}
		return out
	}

	t.Run("loopback_filters_ultra_and_respects_known_empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer k1")
		rec := httptest.NewRecorder()
		serveModelListWithRoutes(rec, req, "k1", []string{"gpt-6-flagship", "plain-fixed", "opus"}, map[string]protocol.ModelInfo{
			"gpt-6-flagship": {
				ID:                    "gpt-6-flagship",
				ReasoningLevels:       protocol.GPTReasoningLevels(),
				DefaultReasoningLevel: "ultra",
				ReasoningKnown:        true,
			},
			"plain-fixed": {
				ID:             "plain-fixed",
				ReasoningKnown: true,
			},
		}, map[string]string{"opus": "claude-opus-4-6"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		raw := rec.Body.Bytes()
		gpt := gjson.GetBytes(raw, `data.#(id=="gpt-6-flagship")`)
		if !gpt.Get("supports_reasoning_effort").Bool() {
			t.Fatalf("gpt-6-flagship missing supports_reasoning_effort=true: %s", gpt.Raw)
		}
		gotGrok := extractEffortValues(gpt)
		wantGrok := []string{"low", "medium", "high", "xhigh", "max"}
		if !slices.Equal(gotGrok, wantGrok) {
			t.Fatalf("gpt-6-flagship Grok reasoning_efforts = %v, want %v (ultra must be filtered)", gotGrok, wantGrok)
		}
		if gpt.Get("reasoning_effort_server_default").String() != "max" {
			t.Fatalf("ultra default did not clamp to max for Grok: %s", gpt.Raw)
		}
		if gpt.Get("supported_reasoning_levels.#").Int() != 6 {
			t.Fatalf("supported_reasoning_levels dropped ultra for Codex: %s", gpt.Raw)
		}

		plain := gjson.GetBytes(raw, `data.#(id=="plain-fixed")`)
		if plain.Get("supports_reasoning_effort").Type != gjson.False || plain.Get("reasoning_efforts").Exists() {
			t.Fatalf("plain-fixed should emit supports_reasoning_effort=false without efforts: %s", plain.Raw)
		}

		opus := gjson.GetBytes(raw, `data.#(id=="opus")`)
		if !opus.Get("supports_reasoning_effort").Bool() || !slices.Equal(extractEffortValues(opus), []string{"low", "medium", "high", "max"}) {
			t.Fatalf("routed alias opus did not resolve claude-opus-4-6 efforts: %s", opus.Raw)
		}
	})

	t.Run("xai_qoder_kiro_kimi_cursor_gemini_workbuddy_autoclaw", func(t *testing.T) {
		// 1. xAI / Grok catalog parsing + fallback.
		xaiParsed, err := parseXaiModels([]byte(`{"data":[
			{"id":"grok-4.6","api_backend":"responses","supports_reasoning_effort":true,"reasoning_effort_server_default":"high","reasoning_efforts":[{"value":"low","label":"Low"},{"value":"medium","label":"Medium"},{"value":"high","label":"High","default":true},{"value":"xhigh","label":"Extra High"}]},
			{"id":"grok-4.20-0309-non-reasoning","api_backend":"responses"}
		]}`))
		if err != nil {
			t.Fatal(err)
		}
		xaiDetails := xaiModelDetails("", xaiParsed)
		if len(xaiDetails["grok-4.6"].ReasoningLevels) != 4 || xaiDetails["grok-4.6"].DefaultReasoningLevel != "high" {
			t.Fatalf("xai grok-4.6 reasoning details = %+v", xaiDetails["grok-4.6"])
		}
		if !xaiDetails["grok-4.20-0309-non-reasoning"].ReasoningKnown || len(xaiDetails["grok-4.20-0309-non-reasoning"].ReasoningLevels) != 0 {
			t.Fatalf("xai non-reasoning model acquired efforts: %+v", xaiDetails["grok-4.20-0309-non-reasoning"])
		}
		for _, fb := range xaiFallbackModels() {
			if !fb.ReasoningKnown || len(fb.ReasoningLevels) == 0 {
				t.Fatalf("xaiFallbackModels() missing reasoning efforts on %s: %+v", fb.ID, fb)
			}
		}

		// 2. Qoder /v1/models.
		qSvc := &qoderService{
			apiKey: "qk",
			models: qoderModelsFromCatalog([]map[string]any{
				{"key": "kmodel", "enable": true, "display_name": "Kimi", "thinking_config": map[string]any{"enabled": map[string]any{"efforts": map[string]any{"low": map[string]any{}, "high": map[string]any{}, "max": map[string]any{"is_default": true}}}}},
				{"key": "qtoggle", "enable": true, "display_name": "Qwen Toggle", "thinking_config": map[string]any{"enabled": map[string]any{"description": "toggle"}}},
			}),
		}
		qReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		qReq.Header.Set("Authorization", "Bearer qk")
		qRec := httptest.NewRecorder()
		qSvc.handleModels(qRec, qReq)
		if !gjson.GetBytes(qRec.Body.Bytes(), `data.#(id=="kmodel").supports_reasoning_effort`).Bool() ||
			!slices.Equal(extractEffortValues(gjson.GetBytes(qRec.Body.Bytes(), `data.#(id=="kmodel")`)), []string{"low", "high", "max"}) {
			t.Fatalf("Qoder kmodel /v1/models missing reasoning_efforts: %s", qRec.Body.String())
		}
		if gjson.GetBytes(qRec.Body.Bytes(), `data.#(id=="qtoggle").supports_reasoning_effort`).Type != gjson.False {
			t.Fatalf("Qoder toggle-only model should have supports_reasoning_effort=false: %s", qRec.Body.String())
		}

		// 3. Kiro initial details + /v1/models.
		kiroDetails := kiroInitialModelDetails("claude-sonnet-4-6,claude-haiku-4-5", []string{"claude-sonnet-4-6", "claude-haiku-4-5"})
		if !kiroDetails["claude-sonnet-4-6"].ReasoningKnown || len(kiroDetails["claude-sonnet-4-6"].ReasoningLevels) != 4 {
			t.Fatalf("Kiro claude-sonnet-4-6 details = %+v", kiroDetails["claude-sonnet-4-6"])
		}
		if !kiroDetails["claude-haiku-4-5"].ReasoningKnown || len(kiroDetails["claude-haiku-4-5"].ReasoningLevels) != 0 {
			t.Fatalf("Kiro claude-haiku-4-5 should be known empty effort list: %+v", kiroDetails["claude-haiku-4-5"])
		}

		// 4. Kimi, Cursor, WorkBuddy, AutoClaw, Gemini, Mixed.
		kimiDetails := kimiModelDetails([]runtimeModelRoute{
			{Name: "kimi-for-coding", Alias: "kimi-for-coding"},
			{Name: "kimi-for-coding-highspeed", Alias: "kimi-for-coding-highspeed"},
		})
		if len(kimiDetails["kimi-for-coding"].ReasoningLevels) != 3 || len(kimiDetails["kimi-for-coding-highspeed"].ReasoningLevels) != 0 || !kimiDetails["kimi-for-coding-highspeed"].ReasoningKnown {
			t.Fatalf("Kimi reasoning details mismatch: %+v", kimiDetails)
		}

		curDetails := cursorModelDetails([]string{"claude-sonnet-4-6"}, map[string]string{"claude-sonnet-4-6": "claude-sonnet-4-6"})
		if len(curDetails["claude-sonnet-4-6"].ReasoningLevels) != 4 {
			t.Fatalf("Cursor claude-sonnet-4-6 reasoning details = %+v", curDetails["claude-sonnet-4-6"])
		}

		_, wbModels, _, wbDetails := buildWorkBuddyRoutes("", []workbuddyModel{
			{ID: "wb-reasoner", Name: "WB Reasoner", SupportsReasoning: true},
			{ID: "claude-opus-4-6", Name: "Claude Opus 4.6", SupportsReasoning: false},
			{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", SupportsReasoning: true},
		})
		if wbDetails["wb-reasoner"].ReasoningKnown || len(wbDetails["wb-reasoner"].ReasoningLevels) != 3 ||
			wbDetails["claude-opus-4-6"].ReasoningKnown || len(wbDetails["claude-opus-4-6"].ReasoningLevels) != 0 {
			t.Fatalf("WorkBuddy discovery must keep ReasoningKnown=false and preserve SupportsReasoning fallback: %+v", wbDetails)
		}
		wbHome := t.TempDir()
		t.Setenv("HOME", wbHome)
		t.Setenv("USERPROFILE", wbHome)
		if cachePath, err := modelsdev.CachePath(); err != nil {
			t.Fatal(err)
		} else if catalogBytes, err := os.ReadFile(filepath.Join("..", "modelsdev", "modelsdev.json")); err != nil {
			t.Fatal(err)
		} else if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(cachePath, catalogBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		enrichWorkBuddyModelDetails(wbDetails)
		wbReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		wbReq.Header.Set("Authorization", "Bearer wk")
		wbRec := httptest.NewRecorder()
		serveModelListWithRoutes(wbRec, wbReq, "wk", wbModels, wbDetails, nil)
		opusEntry := gjson.GetBytes(wbRec.Body.Bytes(), `data.#(id=="claude-opus-4-6")`)
		sonnetEntry := gjson.GetBytes(wbRec.Body.Bytes(), `data.#(id=="claude-sonnet-4-6")`)
		reasonerEntry := gjson.GetBytes(wbRec.Body.Bytes(), `data.#(id=="wb-reasoner")`)
		if !opusEntry.Get("supports_reasoning_effort").Bool() ||
			!slices.Equal(extractEffortValues(opusEntry), []string{"low", "medium", "high", "max"}) {
			t.Fatalf("WorkBuddy /v1/models claude-opus-4-6 effort resolution = %s", wbRec.Body.String())
		}
		if !sonnetEntry.Get("supports_reasoning_effort").Bool() ||
			!slices.Equal(extractEffortValues(sonnetEntry), []string{"low", "medium", "high", "max"}) {
			t.Fatalf("WorkBuddy /v1/models claude-sonnet-4-6 effort resolution = %s", wbRec.Body.String())
		}
		if !reasonerEntry.Get("supports_reasoning_effort").Bool() ||
			!slices.Equal(extractEffortValues(reasonerEntry), []string{"low", "medium", "high"}) {
			t.Fatalf("WorkBuddy /v1/models wb-reasoner fallback = %s", wbRec.Body.String())
		}

		gemCat := &geminiCatalog{Models: GeminiModelCatalog()}
		gemIDs, _, gemDetails := gemCat.publicModels()
		gemServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveModelListWithDetails(w, r, "gk", gemIDs, gemDetails)
		}))
		defer gemServer.Close()
		roundTrip, err := protocol.GetOpenAIModelInfos(gemServer.URL+"/v1", "gk")
		if err != nil {
			t.Fatalf("round-trip GetOpenAIModelInfos failed: %v", err)
		}
		foundArgon, foundClaude := false, false
		for _, info := range roundTrip {
			if info.ID == "gemini-4-argon" && info.ReasoningKnown && len(info.ReasoningLevels) == 3 {
				foundArgon = true
			}
			if info.ID == "claude-opus-4-6-thinking" && info.ReasoningKnown && len(info.ReasoningLevels) == 4 {
				foundClaude = true
			}
		}
		if !foundArgon || !foundClaude {
			t.Fatalf("Gemini /v1/models round-trip missing efforts: %+v", roundTrip)
		}
	})
}

func TestXaiRuntimeClampsUnsupportedReasoningEffort(t *testing.T) {
	var seen []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen = append(seen, payload)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_xai_1\",\"object\":\"response\",\"model\":\"grok\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_xai_1\",\"object\":\"response\",\"model\":\"grok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()

	details := map[string]protocol.ModelInfo{
		"grok-4.6": {
			ID:                    "grok-4.6",
			ReasoningKnown:        true,
			DefaultReasoningLevel: "high",
			ReasoningLevels: []protocol.ReasoningLevel{
				{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}, {Effort: "xhigh"},
			},
		},
		"grok-4.5": {
			ID:                    "grok-4.5",
			ReasoningKnown:        true,
			DefaultReasoningLevel: "high",
			ReasoningLevels: []protocol.ReasoningLevel{
				{Effort: "low"}, {Effort: "medium"}, {Effort: "high"},
			},
		},
		"grok-nonreasoning": {
			ID:             "grok-nonreasoning",
			ReasoningKnown: true,
		},
	}
	routes := []runtimeModelRoute{
		{Alias: "grok-4.6", Name: "grok-4.6"},
		{Alias: "grok-4.5", Name: "grok-4.5"},
		{Alias: "grok-nonreasoning", Name: "grok-nonreasoning"},
	}

	xaiSvc := newXaiResponsesService("xk", upstream.URL, routes, &codexStaticAuthorizer{token: "up"}, NewUsageTracker())
	xaiSvc.modelDetails = details

	postReq := func(handler http.Handler, key, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/responses status=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	// Passthrough: grok-4.6 ultra -> xhigh
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-4.6","stream":false,"reasoning":{"effort":"ultra"},"input":[{"role":"user","content":"hi"}]}`)
	// Converted (custom tool): grok-4.6 ultra -> xhigh
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-4.6","stream":false,"reasoning":{"effort":"ultra"},"tools":[{"type":"custom","name":"exec"}],"input":[{"role":"user","content":"hi"}]}`)
	// Passthrough: grok-4.5 xhigh -> high
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-4.5","stream":false,"reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":"hi"}]}`)
	// Converted (computer tool): grok-4.5 xhigh -> high
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-4.5","stream":false,"reasoning":{"effort":"xhigh"},"tools":[{"type":"computer_use_preview"}],"input":[{"role":"user","content":"hi"}]}`)
	// Passthrough & converted: grok-nonreasoning strips reasoning object
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-nonreasoning","stream":false,"reasoning":{"effort":"high"},"input":[{"role":"user","content":"hi"}]}`)
	postReq(xaiSvc.handler(), "xk", `{"model":"grok-nonreasoning","stream":false,"reasoning":{"effort":"high"},"tools":[{"type":"custom","name":"exec"}],"input":[{"role":"user","content":"hi"}]}`)

	// Non-xAI service must leave passthrough reasoning untouched.
	codexSvc := newCodexResponsesService("ck", upstream.URL, routes, &codexStaticAuthorizer{token: "up"}, NewUsageTracker())
	codexSvc.modelDetails = details
	postReq(codexSvc.handler(), "ck", `{"model":"grok-4.6","stream":false,"reasoning":{"effort":"ultra"},"input":[{"role":"user","content":"hi"}]}`)

	if len(seen) != 7 {
		t.Fatalf("captured %d upstream requests, want 7", len(seen))
	}
	getEffort := func(p map[string]any) (string, bool) {
		r, ok := p["reasoning"].(map[string]any)
		if !ok {
			return "", false
		}
		eff, _ := r["effort"].(string)
		return eff, true
	}
	if eff, ok := getEffort(seen[0]); !ok || eff != "xhigh" {
		t.Fatalf("xai passthrough ultra = (%q, %v), want xhigh", eff, ok)
	}
	if eff, ok := getEffort(seen[1]); !ok || eff != "xhigh" {
		t.Fatalf("xai converted ultra = (%q, %v), want xhigh", eff, ok)
	}
	if eff, ok := getEffort(seen[2]); !ok || eff != "high" {
		t.Fatalf("xai passthrough grok-4.5 xhigh = (%q, %v), want high", eff, ok)
	}
	if eff, ok := getEffort(seen[3]); !ok || eff != "high" {
		t.Fatalf("xai converted grok-4.5 xhigh = (%q, %v), want high", eff, ok)
	}
	if _, ok := getEffort(seen[4]); ok {
		t.Fatalf("xai passthrough nonreasoning did not strip reasoning: %+v", seen[4])
	}
	if _, ok := getEffort(seen[5]); ok {
		t.Fatalf("xai converted nonreasoning did not strip reasoning: %+v", seen[5])
	}
	if eff, ok := getEffort(seen[6]); !ok || eff != "ultra" {
		t.Fatalf("non-xai codex passthrough modified reasoning effort: (%q, %v), want ultra", eff, ok)
	}
}
