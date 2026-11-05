package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexToolInstructionsMapOnlyDeclaredExecutorNames(t *testing.T) {
	raw, registry, err := toMessages(responseRequest{
		Instructions: "Use functions.exec to call tools.view_image; use mcp__cua_repl.js for the browser.",
		Tools: []map[string]any{
			{"type": "custom", "name": "exec"},
			{"type": "namespace", "name": "mcp__cua_repl", "tools": []any{map[string]any{"type": "function", "name": "js"}}},
		},
	}, []map[string]any{{"role": "user", "content": "inspect this image"}}, Route{Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	system, _ := body["system"].(string)
	for _, pair := range [][2]string{{"exec", toolWireName("", "exec")}, {"mcp__cua_repl.js", toolWireName("mcp__cua_repl", "js")}} {
		if !strings.Contains(system, fmt.Sprintf("%q => %q", pair[0], pair[1])) {
			t.Fatalf("missing tool name translation for %q", pair[0])
		}
	}
	if !strings.Contains(system, "input string containing the complete raw tool input") || !strings.Contains(system, "not additional callable functions") || !strings.Contains(system, "prefix an unnamespaced tool with functions.") {
		t.Fatal("executor boundary or custom input contract is missing")
	}
	if len(registry) != 2 || len(body["tools"].([]any)) != 2 {
		t.Fatal("documentation created an extra callable tool")
	}
	if toolCallingInstructions(nil, nil) != "" {
		t.Fatal("tool-free summarization acquired a tool protocol")
	}
}

func TestCodexExecutorDocumentationDoesNotAuthorizeNestedTools(t *testing.T) {
	for _, name := range []string{"view_image", "exec_f4c54ead4764"} {
		t.Run(name, func(t *testing.T) {
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				writeMessage(w, []map[string]any{{"type": "tool_use", "id": "invalid-call", "name": name, "input": map[string]any{"input": "private-tool-argument"}}})
			}, "anthropic", Options{})
			response := postResponses(t, g, map[string]any{
				"model": "ccl/chosen/model-a", "stream": true,
				"instructions": "Use tools.view_image through functions.exec.",
				"tools":        []any{map[string]any{"type": "custom", "name": "exec"}},
				"input":        "inspect the image",
			})
			defer response.Body.Close()
			raw, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(raw), "outside the declared tool list") || strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "response.output_item.added") || strings.Contains(string(raw), "private-tool-argument") {
				t.Fatalf("undeclared tool was emitted or its arguments leaked: %s", raw)
			}
		})
	}
}

func TestCodexToolModeSwitchKeepsHistoryWithoutRetiredCalls(t *testing.T) {
	request := responseRequest{Tools: []map[string]any{{"type": "custom", "name": "exec"}}}
	items := []map[string]any{
		{"type": "function_call", "call_id": "old-shell", "name": "exec_command", "arguments": `{"cmd":"old-command"}`},
		{"type": "function_call_output", "call_id": "old-shell", "output": "old-command-result"},
		{"type": "function_call", "call_id": "old-cua", "namespace": "mcp__cua_repl", "name": "js", "arguments": `{"code":"old-cua-read"}`},
		{"type": "function_call_output", "call_id": "old-cua", "output": []any{map[string]any{"type": "input_text", "text": "old-ui-state"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,c2NyZWVu"}}},
		{"type": "custom_tool_call", "call_id": "current-exec", "name": "exec", "input": "current-script"},
		{"type": "custom_tool_call_output", "call_id": "current-exec", "output": "current-result"},
	}
	raw, registry, err := toMessages(request, items, Route{Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	toolCalls, toolResults, images := 0, 0, 0
	for _, rawMessage := range body["messages"].([]any) {
		for _, rawBlock := range rawMessage.(map[string]any)["content"].([]any) {
			block := rawBlock.(map[string]any)
			switch block["type"] {
			case "tool_use":
				toolCalls++
				if _, ok := registry[textValue(block["name"])]; !ok || block["id"] != "current-exec" {
					t.Fatal("retired history was replayed as a callable tool")
				}
			case "tool_result":
				toolResults++
				if block["tool_use_id"] != "current-exec" {
					t.Fatal("retired result was replayed without a matching tool call")
				}
			case "image":
				images++
			}
		}
	}
	if toolCalls != 1 || toolResults != 1 || images != 1 {
		t.Fatalf("history structure lost: calls=%d results=%d images=%d", toolCalls, toolResults, images)
	}
	for _, want := range []string{"old-command", "old-command-result", "old-cua-read", "old-ui-state", "current-script", "current-result"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("history lost %q", want)
		}
	}
}

func TestCodexDesktopNotificationDoesNotInventToolCall(t *testing.T) {
	item := map[string]any{"type": "function_call_output", "id": "fco_notification", "name": "send_message_to_thread", "namespace": "codex_app", "output": "read-only-verification-request"}
	raw, _, err := toMessages(responseRequest{}, []map[string]any{item}, Route{Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tool_result") || strings.Contains(string(raw), "tool_use_id") || !strings.Contains(string(raw), "codex_app.send_message_to_thread") || !strings.Contains(string(raw), "read-only-verification-request") {
		t.Fatalf("desktop notification was lost or correlated to an invented call: %s", raw)
	}
	delete(item, "name")
	if _, _, err := toMessages(responseRequest{}, []map[string]any{item}, Route{Model: "fixture"}); err == nil {
		t.Fatal("unnamed, uncorrelated tool output was accepted")
	}
}

func TestCodexRetiredHistoryDoesNotAuthorizeProviderTool(t *testing.T) {
	oldWire := toolWireName("", "exec_command")
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeMessage(w, []map[string]any{{"type": "tool_use", "id": "rejected-call", "name": oldWire, "input": map[string]any{"cmd": "private-argument-fixture"}}})
	}, "anthropic", Options{})
	response := postResponses(t, g, map[string]any{
		"model": "ccl/chosen/model-a", "stream": true,
		"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
		"input": []any{
			map[string]any{"type": "function_call", "name": "exec_command", "call_id": "retired-call", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "retired-call", "output": "past-result"},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "response.failed") || !strings.Contains(string(raw), oldWire) || !strings.Contains(string(raw), toolWireName("", "exec")) {
		t.Fatalf("missing bounded rejection diagnostics: %s", raw)
	}
	if strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "response.output_item.added") || strings.Contains(string(raw), "private-argument-fixture") {
		t.Fatalf("retired tool was emitted or its arguments leaked: %s", raw)
	}
}
