package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func writeMCPConfig(path, cclExe, sock string, servers []mcpServer, errw io.Writer) error {
	out := map[string]any{
		"cclperm": map[string]any{
			"command": cclExe,
			"args":    []string{"acp-permission"},
			"env": map[string]string{
				"CCL_ACP_PERM_SOCK": sock,
			},
		},
	}
	for _, srv := range servers {
		name := strings.TrimSpace(srv.Name)
		if name == "" || name == "cclperm" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(srv.Type)) {
		case "", "stdio":
			env := map[string]string{}
			for _, e := range srv.Env {
				if e.Name == "" {
					continue
				}
				env[e.Name] = e.Value
			}
			entry := map[string]any{
				"command": srv.Command,
			}
			if srv.Args != nil {
				entry["args"] = srv.Args
			}
			if len(env) > 0 {
				entry["env"] = env
			}
			out[name] = entry
		case "http":
			headers := map[string]string{}
			for _, h := range srv.Headers {
				if h.Name == "" {
					continue
				}
				headers[h.Name] = h.Value
			}
			entry := map[string]any{
				"type": "http",
				"url":  srv.URL,
			}
			if len(headers) > 0 {
				entry["headers"] = headers
			}
			out[name] = entry
		case "sse":
			logf(errw, "ignoring SSE MCP server %s", name)
		default:
			logf(errw, "ignoring MCP server %s with type %s", name, srv.Type)
		}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": out})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func logf(errw io.Writer, format string, args ...any) {
	if errw == nil {
		return
	}
	fmt.Fprintf(errw, "[acp] "+format+"\n", args...)
}

func cclExecutable() string {
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		return "ccl"
	}
	return exe
}
