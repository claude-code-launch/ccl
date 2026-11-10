package codex

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexExecutorDocumentationDoesNotAuthorizeNestedTools(t *testing.T) {
	for _, name := range []string{"view_image", "exec_f4c54ead4764"} {
		t.Run(name, func(t *testing.T) {
			g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
				writeChatTool(w, r, "invalid-call", name, map[string]any{"input": "private-tool-argument"})
			}, "openai", Options{})
			response := postResponses(t, g, map[string]any{
				"model": "ccl/chosen/model-a", "stream": true,
				"instructions": "Use tools.view_image through functions.exec.",
				"tools":        []any{map[string]any{"type": "custom", "name": "exec"}},
				"input":        "inspect the image",
			})
			defer response.Body.Close()
			raw, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(raw), "undeclared tool name") || strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "response.output_item.added") || strings.Contains(string(raw), "private-tool-argument") {
				t.Fatalf("undeclared tool was emitted or its arguments leaked: %s", raw)
			}
		})
	}
}

func TestCodexRetiredHistoryDoesNotAuthorizeProviderTool(t *testing.T) {
	oldWire := "exec_command"
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeChatTool(w, r, "rejected-call", oldWire, map[string]any{"cmd": "private-argument-fixture"})
	}, "openai", Options{})
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
	if !strings.Contains(string(raw), "response.failed") || !strings.Contains(string(raw), "undeclared tool name") {
		t.Fatalf("missing bounded rejection diagnostics: %s", raw)
	}
	if strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "response.output_item.added") || strings.Contains(string(raw), "private-argument-fixture") {
		t.Fatalf("retired tool was emitted or its arguments leaked: %s", raw)
	}
}
