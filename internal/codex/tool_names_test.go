package codex

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexKeepsSimpleRootToolNames(t *testing.T) {
	for _, name := range []string{"exec", "wait", "request_user_input", "tool_search", "exec_command"} {
		if got := toolWireName("", name); got != name {
			t.Fatalf("root tool %q renamed to %q", name, got)
		}
	}
	seen := map[string]bool{}
	for _, pair := range [][2]string{
		{"", "js"}, {"mcp__cua_repl", "js"}, {"another_namespace", "js"},
		{"", "js_0b26852a6f69"}, {"", "bad/name"}, {"", strings.Repeat("x", 100)},
	} {
		wire := toolWireName(pair[0], pair[1])
		if !simpleToolName(wire) || seen[wire] {
			t.Fatalf("unsafe or colliding tool name for %v: %q", pair, wire)
		}
		seen[wire] = true
	}
}

func TestCodexMalformedToolNameDoesNotExposeReasoning(t *testing.T) {
	const malformed = "exec_f4d... wait typo private-reasoning <｜end▁of▁thinking｜>\n<｜DSML｜calls>"
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeMessage(w, []map[string]any{{"type": "tool_use", "id": "bad-call", "name": malformed, "input": map[string]any{"input": "private-arguments"}}})
	}, "anthropic", Options{})
	response := postResponses(t, g, map[string]any{
		"model": "ccl/chosen/model-a", "stream": true, "input": "fixture",
		"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
	})
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "malformed tool name omitted") || !strings.Contains(string(raw), "response.failed") {
		t.Fatalf("missing malformed-call error: %s", raw)
	}
	for _, unwanted := range []string{"private-reasoning", "private-arguments", "DSML", "response.completed", "response.output_item.added"} {
		if strings.Contains(string(raw), unwanted) {
			t.Fatalf("malformed payload leaked or executed: %s", raw)
		}
	}
}
