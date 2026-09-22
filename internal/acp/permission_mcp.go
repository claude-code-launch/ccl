package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

const permSockEnv = "CCL_ACP_PERM_SOCK"

type permSocketRequest struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input"`
}

type permSocketReply struct {
	Behavior     string          `json:"behavior"`
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Message      string          `json:"message,omitempty"`
}

// RunPermissionMCP speaks MCP stdio for Claude Code's --permission-prompt-tool.
// It is reached from cmd.Execute before any config load.
func RunPermissionMCP(in io.Reader, out io.Writer) error {
	sock := os.Getenv(permSockEnv)
	r := bufio.NewReader(in)
	for {
		msg, err := readMCPMessage(r)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(msg)) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			_ = writeMCPMessage(out, rpcResponse{
				JSONRPC: jsonRPCVersion,
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: errParse, Message: "parse error"},
			})
			continue
		}
		if req.Method == "" {
			continue
		}
		if isNotification(req.ID) {
			continue
		}
		if err := handlePermissionMCP(out, sock, req); err != nil {
			return err
		}
	}
}

func handlePermissionMCP(out io.Writer, sock string, req rpcRequest) error {
	switch req.Method {
	case "initialize":
		return writeMCPMessage(out, rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result: marshalRaw(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cclperm", "version": "1"},
			}),
		})
	case "tools/list":
		return writeMCPMessage(out, rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result: marshalRaw(map[string]any{
				"tools": []map[string]any{{
					"name":        "approve",
					"description": "Approve or deny a Claude Code tool call via the ACP client",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tool_name":   map[string]any{"type": "string"},
							"input":       map[string]any{"type": "object"},
							"tool_use_id": map[string]any{"type": "string"},
						},
						"required": []string{"tool_name", "input"},
					},
				}},
			}),
		})
	case "tools/call":
		reply, callErr := callPermissionTool(sock, req.Params)
		text, _ := json.Marshal(reply)
		result := map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
			"isError": callErr != nil,
		}
		return writeMCPMessage(out, rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result:  marshalRaw(result),
		})
	case "notifications/initialized", "initialized":
		return nil
	default:
		return writeMCPMessage(out, rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Error:   &rpcError{Code: errMethodNotFound, Message: "method not found: " + req.Method},
		})
	}
}

func callPermissionTool(sock string, params json.RawMessage) (permSocketReply, error) {
	deny := permSocketReply{Behavior: "deny", Message: "permission denied"}
	var body struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		deny.Message = "invalid tools/call params"
		return deny, err
	}
	ask, err := parsePermissionArgs(body.Arguments)
	if err != nil {
		deny.Message = err.Error()
		return deny, err
	}
	if sock == "" {
		deny.Message = "CCL_ACP_PERM_SOCK is not set"
		return deny, fmt.Errorf("%s", deny.Message)
	}
	reply, err := askACPPermission(sock, ask)
	if err != nil {
		deny.Message = err.Error()
		return deny, err
	}
	if reply.Behavior == "" {
		reply.Behavior = "deny"
		if reply.Message == "" {
			reply.Message = "permission denied"
		}
	}
	return reply, nil
}

func parsePermissionArgs(raw json.RawMessage) (permSocketRequest, error) {
	var ask permSocketRequest
	if len(raw) == 0 {
		return ask, fmt.Errorf("missing permission arguments")
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return ask, err
	}
	ask.ToolName = stringField(generic, "tool_name", "toolName")
	ask.ToolUseID = stringField(generic, "tool_use_id", "toolUseId")
	if ask.ToolName == "" {
		return ask, fmt.Errorf("missing tool_name")
	}
	if input, ok := generic["input"]; ok {
		ask.Input = marshalRaw(input)
	}
	if len(ask.Input) == 0 {
		ask.Input = json.RawMessage(`{}`)
	}
	return ask, nil
}

func askACPPermission(sock string, req permSocketRequest) (permSocketReply, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return permSocketReply{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return permSocketReply{}, err
	}
	var reply permSocketReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return permSocketReply{}, err
	}
	return reply, nil
}

func readMCPMessage(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] == '{' {
			line, err := r.ReadBytes('\n')
			if err != nil && len(bytes.TrimSpace(line)) == 0 {
				return nil, err
			}
			return bytes.TrimSpace(line), nil
		}
		if b[0] == '\n' || b[0] == '\r' {
			if _, err := r.ReadByte(); err != nil {
				return nil, err
			}
			continue
		}
		n := 0
		for {
			line, err := r.ReadString('\n')
			if err != nil && line == "" {
				return nil, err
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				break
			}
			lower := strings.ToLower(trimmed)
			if rest, ok := strings.CutPrefix(lower, "content-length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(rest))
			}
		}
		if n <= 0 {
			return nil, fmt.Errorf("missing Content-Length")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

func writeMCPMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}
