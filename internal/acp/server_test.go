package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("CCL_ACP_FAKE_CLAUDE") == "1" {
		os.Exit(runFakeClaude())
	}
	os.Exit(m.Run())
}

func runFakeClaude() int {
	mode := os.Getenv("CCL_ACP_FAKE_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if path := os.Getenv("CCL_ACP_FAKE_CAPTURE"); path != "" {
			_ = os.WriteFile(path, raw, 0o600)
		}
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source *struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Type != "user" {
			continue
		}
		var text strings.Builder
		for _, block := range msg.Message.Content {
			text.WriteString(block.Text)
		}
		if mode == "hang" || strings.Contains(text.String(), "HANG") {
			select {}
		}
		if mode == "wait" {
			path := os.Getenv("CCL_ACP_FAKE_RELEASE")
			for {
				if _, err := os.Stat(path); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		switch mode {
		case "max_turns":
			fmt.Println(`{"type":"result","subtype":"error_max_turns","session_id":"claude-sess-1"}`)
		case "no_session":
			fmt.Println(`{"type":"result","subtype":"success","is_error":false}`)
		case "edit":
			fmt.Println(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Edit","input":{"file_path":"a.txt","old_string":"old","new_string":"new"}}]}}`)
			fmt.Println(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`)
			fmt.Println(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-1"}`)
		case "perm":
			runFakePermission()
		default:
			fmt.Println(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-1"}`)
		}
	}
	if err := scanner.Err(); err != nil {
		return 1
	}
	return 0
}

func runFakePermission() {
	sock := mcpSockFromArgs()
	if sock == "" {
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-1"}`)
		return
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-1"}`)
		return
	}
	_ = json.NewEncoder(conn).Encode(map[string]any{
		"tool_name":   "Edit",
		"tool_use_id": "toolu_perm",
		"input": map[string]any{
			"file_path":  "a.txt",
			"old_string": "old",
			"new_string": "new",
		},
	})
	var reply permSocketReply
	_ = json.NewDecoder(conn).Decode(&reply)
	_ = conn.Close()
	if reply.Behavior == "allow" {
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_perm","name":"Edit","input":{"file_path":"a.txt","old_string":"old","new_string":"new"}}]}}`)
		fmt.Println(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_perm","content":"ok"}]}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"edited"}]}}`)
	}
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-1"}`)
}

func mcpSockFromArgs() string {
	for i, a := range os.Args {
		if a == "--mcp-config" && i+1 < len(os.Args) {
			data, err := os.ReadFile(os.Args[i+1])
			if err != nil {
				return ""
			}
			var cfg struct {
				MCPServers map[string]struct {
					Env map[string]string `json:"env"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &cfg); err != nil {
				return ""
			}
			return cfg.MCPServers["cclperm"].Env[permSockEnv]
		}
	}
	return os.Getenv(permSockEnv)
}

func fakeClaude(t *testing.T, mode string) func(string, []string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return func(cwd string, extra []string) *exec.Cmd {
		cmd := exec.Command(exe, extra...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(),
			"CCL_ACP_FAKE_CLAUDE=1",
			"CCL_ACP_FAKE_MODE="+mode,
		)
		return cmd
	}
}

type acpConn struct {
	inW  *io.PipeWriter
	enc  *json.Encoder
	dec  *json.Decoder
	stop func()
}

func startACP(t *testing.T, cfg Config) *acpConn {
	t.Helper()
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, inR, outW, cfg)
		_ = outW.Close()
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = inW.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Serve did not exit")
			}
		})
	}
	t.Cleanup(stop)
	return &acpConn{
		inW:  inW,
		enc:  json.NewEncoder(inW),
		dec:  json.NewDecoder(outR),
		stop: stop,
	}
}

func (c *acpConn) call(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.enc.Encode(req); err != nil {
		t.Fatalf("encode %s: %v", method, err)
	}
	return c.readRPC(t)
}

func (c *acpConn) notify(t *testing.T, method string, params any) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if err := c.enc.Encode(req); err != nil {
		t.Fatalf("notify %s: %v", method, err)
	}
}

func (c *acpConn) readRPC(t *testing.T) map[string]any {
	t.Helper()
	type res struct {
		msg map[string]any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var msg map[string]any
		err := c.dec.Decode(&msg)
		ch <- res{msg, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("decode: %v", r.err)
		}
		return r.msg
	case <-time.After(8 * time.Second):
		t.Fatal("timeout waiting for RPC")
		return nil
	}
}

func rpcErrorCode(msg map[string]any) int {
	errObj, _ := msg["error"].(map[string]any)
	code, _ := errObj["code"].(float64)
	return int(code)
}

func TestInitialize(t *testing.T) {
	c := startACP(t, Config{AgentVersion: "testdev", NewCommand: fakeClaude(t, "")})
	msg := c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	result, _ := msg["result"].(map[string]any)
	if result["protocolVersion"] != float64(1) {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	info, _ := result["agentInfo"].(map[string]any)
	if info["name"] != "ccl" || info["version"] != "testdev" {
		t.Fatalf("agentInfo = %v", info)
	}
	caps, _ := result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("loadSession = %v", caps["loadSession"])
	}
	promptCaps, _ := caps["promptCapabilities"].(map[string]any)
	if promptCaps["embeddedContext"] != true || promptCaps["image"] != true {
		t.Fatalf("promptCapabilities = %v", promptCaps)
	}
	mcpCaps, _ := caps["mcpCapabilities"].(map[string]any)
	if mcpCaps["http"] != true {
		t.Fatalf("mcpCapabilities = %v", mcpCaps)
	}
	sessCaps, _ := caps["sessionCapabilities"].(map[string]any)
	if _, ok := sessCaps["close"].(map[string]any); !ok {
		t.Fatalf("sessionCapabilities.close = %v", sessCaps)
	}
	if msg["id"] != float64(1) {
		t.Fatalf("id = %v", msg["id"])
	}
}

func TestMethodBeforeInitialize(t *testing.T) {
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	msg := c.call(t, 1, "session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErrorCode(msg) != errInvalidRequest {
		t.Fatalf("want -32600, got %v", msg)
	}
}

func TestUnknownMethod(t *testing.T) {
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	msg := c.call(t, 2, "nope/nope", nil)
	if rpcErrorCode(msg) != errMethodNotFound {
		t.Fatalf("want -32601, got %v", msg)
	}
}

func TestSessionNewRequiresAbsCwd(t *testing.T) {
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	for i, cwd := range []string{"", "relative/path"} {
		msg := c.call(t, 10+i, "session/new", map[string]any{"cwd": cwd})
		if rpcErrorCode(msg) != errInvalidParams {
			t.Fatalf("cwd %q: want -32602, got %v", cwd, msg)
		}
	}
}

func TestSessionNewThenPrompt(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{AgentVersion: "testdev", NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{map[string]any{"name": "ignored"}},
	})
	result, _ := created["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("missing sessionId: %v", created)
	}

	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt": []any{
				map[string]any{"type": "image", "text": "skip"},
				map[string]any{"type": "text", "text": "hello"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var updates int
	var prompt map[string]any
	for prompt == nil {
		msg := c.readRPC(t)
		if method, _ := msg["method"].(string); method == "session/update" {
			params, _ := msg["params"].(map[string]any)
			update, _ := params["update"].(map[string]any)
			content, _ := update["content"].(map[string]any)
			if update["sessionUpdate"] != "agent_message_chunk" || content["text"] != "hi" {
				t.Fatalf("unexpected update: %v", msg)
			}
			if _, ok := msg["id"]; ok {
				t.Fatalf("session/update should omit id: %v", msg)
			}
			updates++
			continue
		}
		prompt = msg
	}
	if updates != 1 {
		t.Fatalf("updates = %d", updates)
	}
	if prompt["id"] != float64(3) {
		t.Fatalf("prompt id = %v", prompt["id"])
	}
	pres, _ := prompt["result"].(map[string]any)
	if pres["stopReason"] != stopEndTurn {
		t.Fatalf("stopReason = %v", pres["stopReason"])
	}
}

func TestStdoutContainsOnlyJSONRPC(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
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
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "Using provider-specific claude config") {
			t.Fatalf("stdout leaked banner: %s", raw)
		}
		if msg["id"] == float64(3) {
			return
		}
	}
}

func TestCancelDuringPrompt(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "hang")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
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
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	msg := c.readRPC(t)
	if rpcErrorCode(msg) != 0 && msg["error"] != nil {
		t.Fatalf("cancel should complete the prompt, not JSON-RPC error: %v", msg)
	}
	if msg["id"] != float64(3) {
		t.Fatalf("id = %v", msg["id"])
	}
	res, _ := msg["result"].(map[string]any)
	if res["stopReason"] != stopCancelled {
		t.Fatalf("stopReason = %v", res)
	}
}

func TestBusySession(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "hang")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	prompt := map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
	}
	if err := c.enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": prompt}); err != nil {
		t.Fatal(err)
	}
	busy := c.call(t, 4, "session/prompt", prompt)
	if rpcErrorCode(busy) != errInternal {
		t.Fatalf("want busy -32603, got %v", busy)
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	done := c.readRPC(t)
	if done["id"] != float64(3) {
		t.Fatalf("expected prompt 3 result, got %v", done)
	}
}

func TestPrepareFailureIsJSONRPCError(t *testing.T) {
	c := startACP(t, Config{
		NewCommand: func(cwd string, extra []string) *exec.Cmd {
			return exec.Command("/no-such-ccl-acp-claude-binary")
		},
	})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	msg := c.call(t, 2, "session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErrorCode(msg) != errInternal {
		t.Fatalf("want -32603, got %v", msg)
	}
}

func TestPromptEmptyText(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	msg := c.call(t, 3, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "image"}},
	})
	if rpcErrorCode(msg) != errInvalidParams {
		t.Fatalf("want -32602, got %v", msg)
	}
}

func TestParseError(t *testing.T) {
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	if _, err := c.inW.Write([]byte("{\n")); err != nil {
		t.Fatal(err)
	}
	msg := c.readRPC(t)
	if rpcErrorCode(msg) != errParse {
		t.Fatalf("want parse error, got %v", msg)
	}
}

func TestCancelIsNotification(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "hang")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
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
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	msg := c.readRPC(t)
	if msg["method"] != nil {
		t.Fatalf("unexpected method on cancel path: %v", msg)
	}
	if msg["id"] != float64(3) {
		t.Fatalf("only the in-flight prompt should reply, got %v", msg)
	}
}

func TestMaxTurnsStopReason(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "max_turns")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
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
	msg := c.readRPC(t)
	if msg["id"] != float64(3) {
		t.Fatalf("id = %v", msg["id"])
	}
	res, _ := msg["result"].(map[string]any)
	if res["stopReason"] != stopMaxTurnRequests {
		t.Fatalf("stopReason = %v", res)
	}
}

func TestPromptAfterCancel(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "HANG"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	cancelled := c.readRPC(t)
	if cancelled["id"] != float64(3) {
		t.Fatalf("cancel result: %v", cancelled)
	}
	res, _ := cancelled["result"].(map[string]any)
	if res["stopReason"] != stopCancelled {
		t.Fatalf("stopReason = %v", res)
	}
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	var prompt map[string]any
	for prompt == nil {
		msg := c.readRPC(t)
		if method, _ := msg["method"].(string); method == "session/update" {
			continue
		}
		prompt = msg
	}
	if prompt["id"] != float64(4) {
		t.Fatalf("second prompt: %v", prompt)
	}
	pres, _ := prompt["result"].(map[string]any)
	if pres["stopReason"] != stopEndTurn {
		t.Fatalf("second stopReason = %v", pres)
	}
}

func TestSessionClose(t *testing.T) {
	cwd := t.TempDir()
	c := startACP(t, Config{NewCommand: fakeClaude(t, "")})
	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	created := c.call(t, 2, "session/new", map[string]any{"cwd": cwd})
	sid := created["result"].(map[string]any)["sessionId"].(string)
	closed := c.call(t, 3, "session/close", map[string]any{"sessionId": sid})
	if closed["error"] != nil {
		t.Fatalf("session/close: %v", closed)
	}
	msg := c.call(t, 4, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
	})
	if rpcErrorCode(msg) != errInvalidParams {
		t.Fatalf("prompt after close: %v", msg)
	}
}
