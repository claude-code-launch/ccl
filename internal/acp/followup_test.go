package acp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditToolCallDiff(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "edit")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "edit it"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	var sawCall, sawUpdate bool
	for {
		msg := c.readRPC(t)
		if method, _ := msg["method"].(string); method == "session/update" {
			params, _ := msg["params"].(map[string]any)
			update, _ := params["update"].(map[string]any)
			switch update["sessionUpdate"] {
			case "tool_call":
				if update["toolCallId"] != "toolu_1" || update["kind"] != "edit" {
					t.Fatalf("tool_call = %v", update)
				}
				content, _ := update["content"].([]any)
				if len(content) == 0 {
					t.Fatalf("missing diff: %v", update)
				}
				diff, _ := content[0].(map[string]any)
				if diff["type"] != "diff" {
					t.Fatalf("content = %v", content)
				}
				wantPath := filepath.Join(cwd, "a.txt")
				if diff["path"] != wantPath || diff["oldText"] != "old" || diff["newText"] != "new" {
					t.Fatalf("diff = %v want path %s", diff, wantPath)
				}
				sawCall = true
			case "tool_call_update":
				if update["toolCallId"] != "toolu_1" || update["status"] != "completed" {
					t.Fatalf("tool_call_update = %v", update)
				}
				sawUpdate = true
			}
			continue
		}
		if msg["id"] == float64(3) {
			if !sawCall || !sawUpdate {
				t.Fatalf("missing tool events call=%v update=%v last=%v", sawCall, sawUpdate, msg)
			}
			return
		}
	}
}

func TestPromptImage(t *testing.T) {
	cwd := t.TempDir()
	capture := filepath.Join(t.TempDir(), "in.json")
	t.Setenv("CCL_ACP_FAKE_CAPTURE", capture)
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt": []any{
				map[string]any{"type": "text", "text": "see"},
				map[string]any{"type": "image", "mimeType": "image/png", "data": "abcd"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		msg := c.readRPC(t)
		if msg["id"] == float64(3) {
			break
		}
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"base64"`) || !strings.Contains(string(raw), `"data":"abcd"`) {
		t.Fatalf("claude user line missing image source: %s", raw)
	}
}

func TestSessionNewWritesMCPConfig(t *testing.T) {
	cwd := t.TempDir()
	var extra []string
	c := startACP(t, Config{
		NewCommand: func(dir string, args []string) *exec.Cmd {
			extra = append([]string(nil), args...)
			return fakeClaude(t, "")(dir, args)
		},
	})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{
		"cwd": cwd,
		"mcpServers": []any{
			map[string]any{"name": "xcode-tools", "command": "/usr/bin/xcrun", "args": []any{"mcpbridge"}, "env": []any{}},
			map[string]any{"type": "http", "name": "docs", "url": "https://example.test/mcp"},
		},
	})
	if created["error"] != nil {
		t.Fatalf("session/new: %v", created)
	}
	cfgPath := flagValue(extra, "--mcp-config")
	if cfgPath == "" {
		t.Fatalf("missing --mcp-config in %v", extra)
	}
	if flagValue(extra, "--permission-prompt-tool") != "mcp__cclperm__approve" {
		t.Fatalf("permission tool: %v", extra)
	}
	for _, a := range extra {
		if a == "--dangerously-skip-permissions" {
			t.Fatal("production ACP must not skip permissions")
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCPServers["cclperm"]; !ok {
		t.Fatalf("missing cclperm: %s", data)
	}
	if cfg.MCPServers["xcode-tools"]["command"] != "/usr/bin/xcrun" {
		t.Fatalf("xcode-tools: %v", cfg.MCPServers["xcode-tools"])
	}
	if cfg.MCPServers["docs"]["type"] != "http" {
		t.Fatalf("docs: %v", cfg.MCPServers["docs"])
	}
}

func TestPermissionAllowOnce(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "perm")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "edit"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	perm := c.readRPC(t)
	if perm["method"] != "session/request_permission" {
		t.Fatalf("want request_permission, got %v", perm)
	}
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      perm["id"],
		"result":  map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}},
	}); err != nil {
		t.Fatal(err)
	}
	var prompt map[string]any
	for prompt == nil {
		msg := c.readRPC(t)
		if msg["id"] == float64(3) {
			prompt = msg
		}
	}
	res, _ := prompt["result"].(map[string]any)
	if res["stopReason"] != stopEndTurn {
		t.Fatalf("stopReason = %v", prompt)
	}
}

func TestPermissionRejectOnce(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "perm")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "edit"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	perm := c.readRPC(t)
	if perm["method"] != "session/request_permission" {
		t.Fatalf("want request_permission, got %v", perm)
	}
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      perm["id"],
		"result":  map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject-once"}},
	}); err != nil {
		t.Fatal(err)
	}
	msg := c.readRPC(t)
	if msg["id"] != float64(3) {
		t.Fatalf("prompt result: %v", msg)
	}
}

func TestPermissionCancel(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "perm")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "edit"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	perm := c.readRPC(t)
	if perm["method"] != "session/request_permission" {
		t.Fatalf("want request_permission, got %v", perm)
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	var msg map[string]any
	for {
		msg = c.readRPC(t)
		if msg["id"] == float64(3) {
			break
		}
	}
	res, _ := msg["result"].(map[string]any)
	if res["stopReason"] != stopCancelled {
		t.Fatalf("stopReason = %v", msg)
	}
}

func TestSessionLoadUnknown(t *testing.T) {
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	msg := c.call(t, 2, "session/load", map[string]any{
		"sessionId":  "missing",
		"cwd":        t.TempDir(),
		"mcpServers": []any{},
	})
	if rpcErrorCode(msg) != errInvalidParams {
		t.Fatalf("want -32602, got %v", msg)
	}
}

func TestSessionLoadResumes(t *testing.T) {
	cwd := t.TempDir()
	var extras [][]string
	c := startACP(t, Config{
		NewCommand: func(dir string, args []string) *exec.Cmd {
			extras = append(extras, append([]string(nil), args...))
			return fakeClaude(t, "")(dir, args)
		},
	})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		msg := c.readRPC(t)
		if msg["id"] == float64(3) {
			break
		}
	}
	loaded := c.call(t, 4, "session/load", map[string]any{
		"sessionId":  sid,
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if loaded["error"] != nil {
		t.Fatalf("session/load: %v", loaded)
	}
	if len(extras) < 2 {
		t.Fatalf("expected respawn, extras=%d", len(extras))
	}
	if flagValue(extras[len(extras)-1], "--resume") != "claude-sess-1" {
		t.Fatalf("resume args = %v", extras[len(extras)-1])
	}
}

func TestSessionLoadAcrossProcess(t *testing.T) {
	cwd := t.TempDir()
	store := t.TempDir()
	var firstExtras [][]string
	c1 := startACP(t, Config{
		SessionStorePath: store,
		NewCommand: func(dir string, args []string) *exec.Cmd {
			firstExtras = append(firstExtras, append([]string(nil), args...))
			return fakeClaude(t, "")(dir, args)
		},
	})
	_ = c1.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c1.call(t, 2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c1.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		msg := c1.readRPC(t)
		if msg["id"] == float64(3) {
			break
		}
	}
	c1.stop()

	var secondExtras [][]string
	c2 := startACP(t, Config{
		SessionStorePath: store,
		NewCommand: func(dir string, args []string) *exec.Cmd {
			secondExtras = append(secondExtras, append([]string(nil), args...))
			return fakeClaude(t, "")(dir, args)
		},
	})
	_ = c2.call(t, 4, "initialize", map[string]any{"protocolVersion": 1})
	loaded := c2.call(t, 5, "session/load", map[string]any{
		"sessionId":  sid,
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if loaded["error"] != nil {
		t.Fatalf("cross-process session/load: %v", loaded)
	}
	if len(firstExtras) == 0 || len(secondExtras) == 0 {
		t.Fatalf("expected both Claude processes, first=%d second=%d", len(firstExtras), len(secondExtras))
	}
	if got := flagValue(secondExtras[len(secondExtras)-1], "--resume"); got != "claude-sess-1" {
		t.Fatalf("cross-process resume args = %v", secondExtras[len(secondExtras)-1])
	}
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
