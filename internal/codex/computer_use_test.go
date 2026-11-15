package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestGrokOAuthUsesConvertedBridgeWithCodeModeAndLocalCompaction(t *testing.T) {
	var compactUpstreamHits atomic.Int32
	var step atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			compactUpstreamHits.Add(1)
			http.Error(w, "xAI does not implement /responses/compact", http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch step.Add(1) {
		case 1:
			// Verify custom `exec` tool + `tool_search` reach the converted bridge cleanly.
			toolsText := textValue(body["tools"])
			if !strings.Contains(toolsText, "exec") || !strings.Contains(toolsText, "tool_search") {
				t.Errorf("expected exec and tool_search tools in converted Grok request, got: %s", toolsText)
			}
			writeChatTool(w, r, "call_exec_1", "exec", map[string]any{"input": "await tools.computer_use_screenshot({})"})
		case 2:
			// Screenshot tool result arrives back cleanly on follow-up turn via local previous_response_id.
			msgText := textValue(body["messages"])
			if !strings.Contains(msgText, "desktop-ready") {
				t.Errorf("expected tool output text in follow-up turn, got: %s", msgText)
			}
			writeChatText(w, r, "clicked-submit-button")
		default:
			// Compaction summarization turn: must have tools stripped and screenshots omitted!
			if tools, _ := body["tools"].([]any); len(tools) != 0 {
				t.Errorf("compaction turn retained live tools: %v", tools)
			}
			msgText := textValue(body["messages"])
			if strings.Contains(msgText, "HUGE_SCREENSHOT_BASE64") || !strings.Contains(msgText, "[image omitted]") {
				t.Errorf("compaction turn did not omit screenshot payload: %s", msgText)
			}
			writeChatText(w, r, "Grok computer-use session summary")
		}
	}))
	t.Cleanup(upstream.Close)

	runtime, err := oauthproxy.StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "fake-xai-token", "grok-4-20")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)

	configured := provider.Provider{Name: "grok-acct", OAuthProvider: "grok", Model: "grok-4-20"}
	g, err := New(context.Background(), runtime, configured, Options{Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)

	g.inheritClientToolMode(Catalog{Models: []Model{{
		"slug":                           "gpt-5.4",
		"tool_mode":                      "code_mode_only",
		"shell_type":                     "unified_exec",
		"node_repl_auto_review_required": true,
	}}})

	entry := g.Models()[0]
	if entry["tool_mode"] != "code_mode_only" || entry["shell_type"] != "unified_exec" || entry["supports_search_tool"] != true || entry["node_repl_disabled"] != false {
		t.Fatalf("grok OAuth model missed computer-use catalog capabilities: %+v", entry)
	}

	alias := entry["slug"].(string)
	codeModeTools := []any{
		map[string]any{"type": "custom", "name": "exec", "description": "Execute JavaScript in local REPL", "format": map[string]any{"type": "text", "syntax": "javascript", "definition": "js"}},
		map[string]any{"type": "tool_search", "execution": "client", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}},
	}
	turn1 := responseObject(t, postResponses(t, g, map[string]any{
		"model": alias,
		"tools": codeModeTools,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "Inspect the screen and click Submit"}},
	}))
	output1, _ := turn1["output"].([]any)
	if len(output1) == 0 {
		t.Fatalf("expected custom_tool_call output on turn 1: %+v", turn1)
	}
	callItem, _ := output1[0].(map[string]any)
	if callItem["type"] != "custom_tool_call" || callItem["name"] != "exec" {
		t.Fatalf("expected custom_tool_call exec, got: %+v", callItem)
	}

	hugeScreenshot := "data:image/png;base64," + strings.Repeat("HUGE_SCREENSHOT_BASE64_", 1024)
	turn2 := responseObject(t, postResponses(t, g, map[string]any{
		"model":                alias,
		"previous_response_id": turn1["id"],
		"tools":                codeModeTools,
		"input": []any{map[string]any{
			"type":    "custom_tool_call_output",
			"call_id": callItem["call_id"],
			"output": []any{
				map[string]any{"type": "input_text", "text": "desktop-ready"},
				map[string]any{"type": "input_image", "image_url": hugeScreenshot},
			},
		}},
	}))
	if turn2["status"] != "completed" {
		t.Fatalf("turn 2 did not complete: %+v", turn2)
	}

	// Remote compaction trigger on Grok OAuth must produce a local ccl1: token without calling /responses/compact.
	compacted := responseObject(t, postResponses(t, g, map[string]any{
		"model":                alias,
		"previous_response_id": turn2["id"],
		"tools":                codeModeTools,
		"input":                []any{map[string]any{"type": "compaction_trigger"}},
	}))
	compactedOut, _ := compacted["output"].([]any)
	if len(compactedOut) != 1 {
		t.Fatalf("expected single compaction item, got: %+v", compacted)
	}
	compactionItem, _ := compactedOut[0].(map[string]any)
	encoded, _ := compactionItem["encrypted_content"].(string)
	if !strings.HasPrefix(encoded, "ccl1:") || compactUpstreamHits.Load() != 0 {
		t.Fatalf("grok OAuth did not compact locally: encoded=%q compactUpstreamHits=%d", encoded, compactUpstreamHits.Load())
	}

	// Explicit /responses/compact endpoint also works locally without hitting upstream /responses/compact.
	compactPayload, _ := json.Marshal(map[string]any{
		"model": alias,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "capture window"},
				map[string]any{"type": "input_image", "image_url": hugeScreenshot},
			}},
		},
	})
	compactReq, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses/compact", bytes.NewReader(compactPayload))
	compactReq.Header.Set("Authorization", "Bearer fake-native-key")
	compactResp, err := http.DefaultClient.Do(compactReq)
	if err != nil {
		t.Fatal(err)
	}
	defer compactResp.Body.Close()
	if compactResp.StatusCode != http.StatusOK || compactUpstreamHits.Load() != 0 {
		bodyBytes, _ := io.ReadAll(compactResp.Body)
		t.Fatalf("/responses/compact failed (%d): %s", compactResp.StatusCode, bodyBytes)
	}
}

func TestGPTOAuthKeepsResponsesPassthroughAndCodeModeCapabilities(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"resp_gpt_native","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"native-gpt-ok"}]}]}`)
		case "/v1/responses/compact":
			_, _ = io.WriteString(w, `{"id":"cmp_gpt_native","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"native-cipher"}]}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	runtime, err := oauthproxy.StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "fake-chatgpt-token", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)

	configured := provider.Provider{Name: "gpt-acct", OAuthProvider: "gpt", Model: "gpt-5.4"}
	g, err := New(context.Background(), runtime, configured, Options{Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)

	g.inheritClientToolMode(Catalog{Models: []Model{{
		"slug":       "gpt-5.4",
		"tool_mode":  "code_mode_only",
		"shell_type": "unified_exec",
	}}})

	entry := g.Models()[0]
	if entry["tool_mode"] != "code_mode_only" || entry["shell_type"] != "unified_exec" || entry["supports_search_tool"] != true {
		t.Fatalf("gpt OAuth lost code_mode_only or search tool support: %+v", entry)
	}

	alias := entry["slug"].(string)
	resp := responseObject(t, postResponses(t, g, map[string]any{"model": alias, "input": "hello"}))
	if resp["id"] != "resp_gpt_native" {
		t.Fatalf("expected passthrough response id, got: %+v", resp)
	}

	compactRaw, _ := json.Marshal(map[string]any{"model": alias, "input": "compact"})
	req, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses/compact", bytes.NewReader(compactRaw))
	req.Header.Set("Authorization", "Bearer fake-native-key")
	compactResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer compactResp.Body.Close()
	if compactResp.StatusCode != http.StatusOK || len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/responses/compact" {
		t.Fatalf("expected passthrough /v1/responses and /v1/responses/compact, got status=%d paths=%v", compactResp.StatusCode, paths)
	}
}

func TestCompactionItemsSanitizesScreenshotsAndSummarizesAllToolTypes(t *testing.T) {
	rawBase64 := "data:image/jpeg;base64," + strings.Repeat("SCREENSHOT_BYTES_", 256)
	items := []map[string]any{
		{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "inspect browser"},
				map[string]any{"type": "input_image", "image_url": rawBase64},
			},
		},
		{"type": "computer_call", "call_id": "cu_1", "action": map[string]any{"type": "click", "x": 42, "y": 99}},
		{"type": "computer_call_output", "call_id": "cu_1", "output": map[string]any{"type": "computer_screenshot", "image_url": rawBase64}},
		{"type": "shell_call", "call_id": "sh_1", "action": map[string]any{"commands": []any{"ls -la"}}},
		{"type": "shell_call_output", "call_id": "sh_1", "output": "README.md"},
		{"type": "apply_patch_call", "call_id": "ap_1", "operation": map[string]any{"type": "update_file", "path": "a.go"}},
		{"type": "apply_patch_call_output", "call_id": "ap_1", "output": "applied"},
		{"type": "tool_search_call", "call_id": "ts_1", "arguments": map[string]any{"query": "computer_use"}},
		{"type": "tool_search_output", "call_id": "ts_1", "tools": []any{map[string]any{"type": "function", "name": "mcp__computer_use__click"}}},
	}

	compacted := compactionItems(items)
	encoded, err := json.Marshal(compacted)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "SCREENSHOT_BYTES_") {
		t.Fatalf("compaction leaked inline screenshot bytes: %s", text)
	}
	for _, want := range []string{
		"inspect browser",
		"[image omitted]",
		"Previous tool call computer (cu_1)",
		"Previous tool call shell (sh_1)",
		"README.md",
		"Previous tool call apply_patch (ap_1)",
		"mcp__computer_use__click",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("compaction missed %q in %s", want, text)
		}
	}
}
