package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPermissionMCPInitializeAndList(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunPermissionMCP(inR, outW)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("RunPermissionMCP did not exit")
		}
	})

	if _, err := fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); err != nil {
		t.Fatal(err)
	}
	initMsg := readMCPFrame(t, outR)
	result, _ := initMsg["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("initialize = %v", initMsg)
	}

	if _, err := fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); err != nil {
		t.Fatal(err)
	}
	listMsg := readMCPFrame(t, outR)
	list, _ := listMsg["result"].(map[string]any)
	tools, _ := list["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list = %v", listMsg)
	}
	name, _ := tools[0].(map[string]any)["name"].(string)
	if name != "approve" {
		t.Fatalf("tool name = %v", tools[0])
	}
}

func TestWriteMCPMessageUsesLineDelimitedJSON(t *testing.T) {
	var out bytes.Buffer
	if err := writeMCPMessage(&out, map[string]any{"jsonrpc": "2.0", "id": 1}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("Content-Length:")) {
		t.Fatalf("MCP response used header framing: %q", out.Bytes())
	}
	if !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
		t.Fatalf("MCP response is not newline terminated: %q", out.Bytes())
	}
	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &msg); err != nil {
		t.Fatalf("MCP response is not JSON: %v", err)
	}
}

func TestRunPermissionMCPToolsCall(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "perm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req permSocketRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(permSocketReply{
			Behavior:     "allow",
			UpdatedInput: req.Input,
		})
	}()

	t.Setenv(permSockEnv, sock)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunPermissionMCP(inR, outW)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("RunPermissionMCP did not exit")
		}
	})

	if _, err := fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Edit","input":{"file_path":"a.txt"}}}}`); err != nil {
		t.Fatal(err)
	}
	msg := readMCPFrame(t, outR)
	result, _ := msg["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tools/call error: %v", msg)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("missing content: %v", msg)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"behavior":"allow"`) {
		t.Fatalf("reply text = %s", text)
	}
}

func readMCPFrame(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	raw, err := readMCPMessage(bufio.NewReader(r))
	if err != nil {
		t.Fatalf("read MCP: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal MCP %s: %v", raw, err)
	}
	return msg
}
