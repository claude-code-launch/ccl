package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteMCPConfigMergesServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	var stderr strings.Builder
	err := writeMCPConfig(path, "/bin/ccl", "/tmp/perm.sock", []mcpServer{
		{Name: "xcode-tools", Command: "/usr/bin/xcrun", Args: []string{"mcpbridge"}},
		{Type: "http", Name: "docs", URL: "https://example.test/mcp"},
		{Type: "sse", Name: "events", URL: "https://example.test/sse"},
		{Name: "cclperm", Command: "ignored"},
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	perm := cfg.MCPServers["cclperm"]
	if perm["command"] != "/bin/ccl" {
		t.Fatalf("cclperm = %v", perm)
	}
	args, _ := perm["args"].([]any)
	if len(args) != 1 || args[0] != "acp-permission" {
		t.Fatalf("cclperm args = %v", perm["args"])
	}
	if cfg.MCPServers["xcode-tools"]["command"] != "/usr/bin/xcrun" {
		t.Fatalf("xcode-tools = %v", cfg.MCPServers["xcode-tools"])
	}
	if cfg.MCPServers["docs"]["type"] != "http" {
		t.Fatalf("docs = %v", cfg.MCPServers["docs"])
	}
	if _, ok := cfg.MCPServers["events"]; ok {
		t.Fatalf("sse server should be ignored: %s", raw)
	}
	if !strings.Contains(stderr.String(), "ignoring SSE MCP server events") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
