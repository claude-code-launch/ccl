package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
)

func TestRealCodexDiscoversAndExecutesDeferredMCPTool(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%v", approved), func(t *testing.T) { realCodexDiscoversMCPTool(t, approved) })
	}
}

func realCodexDiscoversMCPTool(t *testing.T, approved bool) {
	t.Helper()
	executable := os.Getenv("CCL_TEST_CODEX")
	if executable == "" {
		t.Skip("set CCL_TEST_CODEX to the official Codex client")
	}
	executable, _ = filepath.Abs(executable)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the local MCP fixture")
	}
	home := t.TempDir()
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "CODEX_HOME": home, "OPENAI_API_KEY": "fake-native-key", "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9", "NO_PROXY": "127.0.0.1,localhost"} {
		t.Setenv(key, value)
	}
	script := filepath.Join(home, "mcp-fixture.mjs")
	if err := os.WriteFile(script, []byte(`import readline from 'node:readline';
const tools = Array.from({length: 40}, (_, i) => ({name: 'unrelated_' + i, description: 'unrelated-service-documentation '.repeat(200), inputSchema: {type:'object',properties:{}}}));
tools.push({name:'shipping_eta', description:'Look up shipping delivery ETA for an order', inputSchema:{type:'object',properties:{order_id:{type:'string'}},required:['order_id']}});
for await (const line of readline.createInterface({input:process.stdin})) {
 const request = JSON.parse(line); if (request.id === undefined) continue;
 let result;
 if(request.method === 'initialize') result = {protocolVersion:request.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'local-fixture',version:'1.0'}};
 else if(request.method === 'tools/list') result = {tools};
 else if(request.method === 'tools/call') result = {content:[{type:'text',text:'mcp-shipping-result-' + request.params.arguments.order_id}]};
 else result = {};
 process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:request.id,result})+'\n');
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	native, _ := json.Marshal(Catalog{Models: []Model{newModel("native-fixture", "native-fixture", nil, nil)}})
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	nodeTOML, _ := json.Marshal(node)
	scriptTOML, _ := json.Marshal(script)
	config := fmt.Sprintf("[mcp_servers.fixture]\ncommand = %s\nargs = [%s]\n", nodeTOML, scriptTOML)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var sawResult atomic.Bool
	var sawDenial atomic.Bool
	var sawClientSearch atomic.Bool
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "unrelated-service-documentation") {
			t.Error("large MCP descriptions were eagerly loaded")
		}
		reply := func(content []map[string]any) {
			message := map[string]any{"role": "assistant"}
			var text strings.Builder
			var calls []any
			for _, block := range content {
				if block["type"] == "text" {
					text.WriteString(textValue(block["text"]))
				} else if block["type"] == "tool_use" {
					calls = append(calls, map[string]any{"index": len(calls), "id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": textValue(block["input"])}})
				}
			}
			message["content"] = text.String()
			reason := "stop"
			if len(calls) > 0 {
				message["tool_calls"] = calls
				reason = "tool_calls"
			}
			writeChatFixture(w, r, message, reason, 3)
		}
		switch calls.Add(1) {
		case 1:
			reply([]map[string]any{{"type": "text", "text": "Finding the shipping tool."}, {"type": "tool_use", "id": "search_fixture", "name": findTool(t, body, "tool_search"), "input": map[string]any{"query": "shipping_eta", "limit": 1}}})
		case 2:
			reply([]map[string]any{{"type": "tool_use", "id": "call_fixture", "name": findTool(t, body, "shipping_eta"), "input": map[string]any{"order_id": "order-42"}}})
		default:
			if strings.Contains(string(raw), "mcp-shipping-result-order-42") {
				sawResult.Store(true)
			}
			if strings.Contains(string(raw), "requires approval") {
				sawDenial.Store(true)
			}
			reply([]map[string]any{{"type": "text", "text": "tool-discovery-done"}})
		}
	}, "openai", Options{Desktop: true})
	g.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			g.serve(w, r)
			return
		}
		raw, err := readBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var request responseRequest
		_ = json.Unmarshal(raw, &request)
		for _, tool := range request.Tools {
			if tool["type"] == "tool_search" {
				if tool["execution"] != "client" {
					t.Error("search is not client-executed")
				}
			}
		}
		items, _ := inputItems(request.Input)
		for _, item := range items {
			if item["type"] == "tool_search_output" {
				sawClientSearch.Store(true)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.Header.Del("Content-Encoding")
		r.ContentLength = int64(len(raw))
		g.serve(w, r)
	})
	overlay, err := Plan(context.Background(), g, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := []string{"-m", "ccl/chosen/model-a", "-c", `web_search="disabled"`}
	if approved {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "exec", "--json", "--skip-git-repo-check", "Use the fixture shipping_eta MCP tool for order-42 and report the result.")
	command := process.Command(executable, args...)
	command.Dir = home
	process.ConfigureGroup(command)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil || !sawClientSearch.Load() || sawResult.Load() != approved || sawDenial.Load() == approved || calls.Load() != 3 || !strings.Contains(output.String(), "tool-discovery-done") {
			t.Fatalf("official client MCP discovery: %v calls=%d result=%v denial=%v\n%s", err, calls.Load(), sawResult.Load(), sawDenial.Load(), output.String())
		}
	case <-time.After(45 * time.Second):
		process.KillTree(command)
		<-done
		t.Fatalf("official client timed out\n%s", output.String())
	}
}
