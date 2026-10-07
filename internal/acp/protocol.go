package acp

import (
	"encoding/json"
)

const protocolVersion = 1

type initializeParams struct {
	ProtocolVersion any `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	AuthMethods       []authMethod      `json:"authMethods"`
}

type agentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  promptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     mcpCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities sessionCapabilities `json:"sessionCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// sessionCapabilities values are empty objects when supported (ACP schema),
// never booleans.
type sessionCapabilities struct {
	Close struct{} `json:"close"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type mcpEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mcpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mcpServer struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	Command string      `json:"command"`
	Args    []string    `json:"args"`
	Env     []mcpEnvVar `json:"env"`
	URL     string      `json:"url"`
	Headers []mcpHeader `json:"headers"`
}

type newSessionParams struct {
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

type loadSessionParams struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type closeParams struct {
	SessionID string `json:"sessionId"`
}

type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

type sessionUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       contentBlock `json:"content"`
}

type toolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

type toolCallContent struct {
	Type    string        `json:"type"`
	Path    string        `json:"path,omitempty"`
	OldText *string       `json:"oldText,omitempty"`
	NewText string        `json:"newText,omitempty"`
	Content *contentBlock `json:"content,omitempty"`
}

type toolCallUpdate struct {
	SessionUpdate string             `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitempty"`
	Name          string             `json:"name,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Status        string             `json:"status,omitempty"`
	Locations     []toolCallLocation `json:"locations,omitempty"`
	Content       []toolCallContent  `json:"content,omitempty"`
	RawInput      any                `json:"rawInput,omitempty"`
	RawOutput     any                `json:"rawOutput,omitempty"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  toolCallUpdate     `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

type contentBlock struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	URI      string         `json:"uri,omitempty"`
	Name     string         `json:"name,omitempty"`
	MimeType string         `json:"mimeType,omitempty"`
	Data     string         `json:"data,omitempty"`
	Resource *resourceBlock `json:"resource,omitempty"`
}

type resourceBlock struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

func agentMessageChunk(text string) sessionUpdate {
	return sessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       contentBlock{Type: "text", Text: text},
	}
}

func userMessageChunk(text string) sessionUpdate {
	return sessionUpdate{
		SessionUpdate: "user_message_chunk",
		Content:       contentBlock{Type: "text", Text: text},
	}
}

var defaultPermissionOptions = []permissionOption{
	{OptionID: "allow-once", Name: "Allow once", Kind: "allow_once"},
	{OptionID: "allow-always", Name: "Allow always", Kind: "allow_always"},
	{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
	{OptionID: "reject-always", Name: "Reject always", Kind: "reject_always"},
}

func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
