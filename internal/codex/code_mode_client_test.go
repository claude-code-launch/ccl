package codex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRealCodexCodeModeCallsClientMCP(t *testing.T) {
	home, run := isolatedCodexClient(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the local MCP fixture")
	}
	script := filepath.Join(home, "computer-fixture.mjs")
	if err := os.WriteFile(script, []byte(`import readline from 'node:readline';
for await (const line of readline.createInterface({input:process.stdin})) {
 const request=JSON.parse(line); if(request.id===undefined) continue;
 let result={};
 if(request.method==='initialize') result={protocolVersion:request.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'computer-fixture',version:'1'}};
 else if(request.method==='tools/list') result={tools:[{name:'get_app_state',description:'Read the app UI state',annotations:{readOnlyHint:true},inputSchema:{type:'object',properties:{}}}]};
 else if(request.method==='tools/call') result={content:[{type:'text',text:'computer-mcp-state-fixture'}]};
 process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:request.id,result})+'\n');
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeTOML, _ := json.Marshal(node)
	scriptTOML, _ := json.Marshal(script)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[mcp_servers.computer_fixture]\ncommand = %s\nargs = [%s]\n", nodeTOML, scriptTOML)), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var sawResult atomic.Bool
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch calls.Add(1) {
		case 1:
			writeMessage(w, []map[string]any{{"type": "tool_use", "id": "classic_mcp_call", "name": findTool(t, body, "get_app_state"), "input": map[string]any{}}})
			return
		case 2:
			writeMessage(w, []map[string]any{{"type": "text", "text": "classic-computer-done"}})
			return
		case 3:
			for _, rawMessage := range body["messages"].([]any) {
				for _, rawBlock := range rawMessage.(map[string]any)["content"].([]any) {
					block := rawBlock.(map[string]any)
					if block["type"] == "tool_use" && strings.Contains(textValue(block["name"]), "get_app_state") {
						t.Error("classic MCP call survived as an undeclared code-mode tool")
					}
				}
			}
			if !strings.Contains(textValue(body["messages"]), "computer-mcp-state-fixture") {
				t.Error("tool-mode switch dropped prior MCP results")
			}
			var names []string
			for _, raw := range body["tools"].([]any) {
				names = append(names, raw.(map[string]any)["name"].(string))
			}
			t.Logf("code-mode tools: %v", names)
			writeMessage(w, []map[string]any{{"type": "tool_use", "id": "code_mode_call", "name": findTool(t, body, toolWireName("", "exec")), "input": map[string]any{"input": `const tool = ALL_TOOLS.find(x => x.name.includes("get_app_state")); text(await tools[tool.name]({}));`}}})
			return
		}
		sawResult.Store(strings.Contains(textValue(body["messages"]), "computer-mcp-state-fixture"))
		writeMessage(w, []map[string]any{{"type": "text", "text": "code-mode-computer-done"}})
	}, "anthropic", Options{Desktop: true})
	// Begin with the classic harness, then resume that same conversation after
	// loading code mode. A fresh-session-only test cannot catch stale tool calls.
	g.models[0]["supports_search_tool"] = false
	classic, err := Plan(t.Context(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := classic.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	output := run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "--dangerously-bypass-approvals-and-sandbox", "exec", "--json", "--skip-git-repo-check", "Use the computer fixture to read app state.")
	var session string
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Type string `json:"type"`
			ID   string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
			session = event.ID
		}
	}
	if session == "" || !strings.Contains(output, "classic-computer-done") {
		t.Fatalf("classic MCP round trip failed: %s", output)
	}
	if err := classic.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The same catalog field that enables desktop code mode must reach CCL
	// through Plan; changing the test gateway directly would miss this bug.
	native := newModel("native-fixture", "native-fixture", nil, nil)
	native["tool_mode"] = "code_mode_only"
	native["node_repl_auto_review_required"] = true
	g.models[0]["supports_search_tool"] = true
	raw, _ := json.Marshal(Catalog{Models: []Model{native}})
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	applyClientFixture(t, g, home)
	if g.models[0]["tool_mode"] != "code_mode_only" || g.models[0]["node_repl_auto_review_required"] != true {
		t.Fatal("native client tool mode or review requirement was lost")
	}
	output = run("-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`, "--dangerously-bypass-approvals-and-sandbox", "exec", "resume", "--json", "--skip-git-repo-check", session, "Read app state again using the current code-mode tools.")
	if calls.Load() != 4 || !sawResult.Load() || !strings.Contains(output, "code-mode-computer-done") {
		t.Fatalf("code mode did not call client MCP: calls=%d result=%v\n%s", calls.Load(), sawResult.Load(), output)
	}
}
