package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

func (s *server) run(chat *conversation, op *operation, rec record, prompt string) (string, error) {
	if path, err := resolveWorkspace(rec.Workspace); err != nil || path != rec.Workspace {
		return "", errors.New("The workspace moved or is no longer available.")
	}
	profile := filepath.Join(s.recordDir(rec.ID), "grok")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return "", workerError("Could not create the isolated Grok profile.", err)
	}
	launch, err := groklaunch.PrepareBrowser(op.ctx, s.opts.GrokPath, s.opts.Provider, profile)
	if err != nil {
		return "", workerError("Could not start the configured provider.", err)
	}
	defer launch.Close()
	a, err := startAgent(launch.Command("agent", "--no-leader", "stdio"), rec.Workspace, &acpClient{chat: chat, op: op})
	if err != nil {
		return "", workerError("Could not start Grok Build.", err)
	}
	defer a.close()

	setupCtx, stop := context.WithTimeout(op.ctx, 30*time.Second)
	defer stop()
	id, err := a.session(setupCtx, rec)
	if err != nil {
		return "", err
	}
	chat.mu.Lock()
	op.agentID = id
	chat.mu.Unlock()
	if err := s.beginPrompt(chat, op, rec, prompt); err != nil || prompt == "" {
		return "", err
	}

	result, err := a.prompt(op.ctx, id, prompt)
	if err != nil {
		return "", workerError("Grok Build could not complete the turn.", err)
	}
	return string(result.StopReason), nil
}

func workerError(message string, cause error) error {
	oauthproxy.LogErrorEvent("browser_session_error", "stage", message, "error", cause)
	return errors.New(message)
}

type agent struct {
	conn   *acp.ClientSideConnection
	cmd    *exec.Cmd
	input  *os.File
	output *os.File
	done   chan struct{}
}

func startAgent(cmd *exec.Cmd, workspace string, client acp.Client) (*agent, error) {
	input, stdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	stdout, output, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	defer output.Close()
	cmd.Dir, cmd.Stdin, cmd.Stdout = workspace, input, output
	process.ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	conn := acp.NewClientSideConnection(client, stdin, stdout)
	// Protocol diagnostics may contain raw agent messages and tool arguments.
	conn.SetLogger(slog.New(slog.DiscardHandler))
	return &agent{conn: conn, cmd: cmd, input: stdin, output: stdout, done: done}, nil
}

func (a *agent) close() {
	_ = a.input.Close()
	select {
	case <-a.done:
	case <-time.After(2 * time.Second):
		process.KillTree(a.cmd)
		<-a.done
	}
	_ = a.output.Close()
}

func (a *agent) session(ctx context.Context, rec record) (acp.SessionId, error) {
	info, err := a.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		return "", workerError("Grok Build did not initialize its ACP interface.", err)
	}
	if info.ProtocolVersion != acp.ProtocolVersionNumber || !info.AgentCapabilities.LoadSession {
		return "", errors.New("Install a Grok Build version that supports ACP v1 session loading.")
	}
	for _, method := range info.AuthMethods {
		if (method.Agent != nil && method.Agent.Id == "xai.api_key") ||
			(method.EnvVar != nil && method.EnvVar.Id == "xai.api_key") {
			if _, err := a.conn.Authenticate(ctx, acp.AuthenticateRequest{
				MethodId: "xai.api_key", Meta: map[string]any{"headless": true},
			}); err != nil {
				return "", workerError("Grok Build could not authenticate with the local provider runtime.", err)
			}
			break
		}
	}
	if rec.AgentID != "" {
		if _, err := a.conn.LoadSession(ctx, acp.LoadSessionRequest{
			SessionId: rec.AgentID, Cwd: rec.Workspace, McpServers: []acp.McpServer{},
		}); err != nil {
			return "", workerError("Grok Build could not load the saved conversation.", err)
		}
		return rec.AgentID, nil
	}
	session, err := a.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: rec.Workspace, McpServers: []acp.McpServer{}})
	if err != nil {
		return "", workerError("Grok Build could not open the workspace session.", err)
	}
	if session.SessionId == "" {
		return "", errors.New("Grok Build returned an empty session ID.")
	}
	return session.SessionId, nil
}

func (a *agent) prompt(ctx context.Context, id acp.SessionId, text string) (acp.PromptResponse, error) {
	// Give the agent time to acknowledge session/cancel and journal the turn
	// before tearing down its transport, even if the HTTP server is stopping.
	requestCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	stopWatch := context.AfterFunc(ctx, func() {
		go func() {
			_ = a.conn.Cancel(requestCtx, acp.CancelNotification{SessionId: id})
		}()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			_ = a.input.Close()
			stop()
		}
	})
	defer stopWatch()
	if err := ctx.Err(); err != nil {
		return acp.PromptResponse{}, err
	}
	return a.conn.Prompt(requestCtx, acp.PromptRequest{
		SessionId: id, Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: text}}},
	})
}

type acpClient struct {
	chat *conversation
	op   *operation
}

func (c *acpClient) SessionUpdate(_ context.Context, note acp.SessionNotification) error {
	chat := c.chat
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if chat.active != c.op || note.SessionId == "" || note.SessionId != c.op.agentID {
		return nil
	}
	update := note.Update
	var e event
	switch {
	case update.UserMessageChunk != nil && update.UserMessageChunk.Content.Text != nil:
		if c.op.live {
			return nil // The browser has already added the submitted prompt.
		}
		e = event{Kind: "user", Text: update.UserMessageChunk.Content.Text.Text, Append: true}
	case update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil:
		e = event{Kind: "assistant", Text: update.AgentMessageChunk.Content.Text.Text, Append: true}
	case update.ToolCall != nil:
		tool := update.ToolCall
		e = event{Kind: "tool", ID: string(tool.ToolCallId), Text: tool.Title, Status: string(tool.Status)}
	case update.ToolCallUpdate != nil:
		tool := update.ToolCallUpdate
		e = event{Kind: "tool", ID: string(tool.ToolCallId)}
		if tool.Title != nil {
			e.Text = *tool.Title
		}
		if tool.Status != nil {
			e.Status = string(*tool.Status)
		}
	default:
		return nil
	}
	if c.op.live {
		chat.emitLocked(e)
	} else if !chat.loaded {
		c.op.replay = append(c.op.replay, e)
	}
	return nil
}

func (c *acpClient) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	cancelled := acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Cancelled: &acp.RequestPermissionOutcomeCancelled{Outcome: "cancelled"},
	}}
	var allow, deny acp.PermissionOptionId
	for _, opt := range req.Options {
		switch opt.Kind {
		case acp.PermissionOptionKindAllowOnce:
			allow = opt.OptionId
		case acp.PermissionOptionKindRejectOnce:
			deny = opt.OptionId
		}
	}
	title := "Tool request"
	if req.ToolCall.Title != nil && *req.ToolCall.Title != "" {
		title = *req.ToolCall.Title
	}
	var detail string
	input := req.ToolCall.RawInput
	if input == nil && len(req.ToolCall.Content) > 0 {
		input = req.ToolCall.Content
	}
	if input != nil {
		raw, err := json.MarshalIndent(input, "", "  ")
		if err != nil || len(raw) > 128<<10 {
			allow = ""
		} else {
			detail = string(raw)
		}
	} else if req.ToolCall.Title == nil || *req.ToolCall.Title == "" {
		allow = ""
	}
	id := uuid.NewString()
	pending := approval{canAllow: allow != "", decision: make(chan string, 1)}

	chat := c.chat
	chat.mu.Lock()
	if chat.active != c.op || !c.op.live || c.op.ctx.Err() != nil || req.SessionId != c.op.agentID {
		chat.mu.Unlock()
		return cancelled, nil
	}
	chat.pending[id] = pending
	chat.emitLocked(event{Kind: "permission", ID: id, Text: title, Detail: detail, CanAllow: pending.canAllow})
	chat.mu.Unlock()
	defer func() {
		chat.mu.Lock()
		defer chat.mu.Unlock()
		if _, ok := chat.pending[id]; ok {
			delete(chat.pending, id)
			chat.emitLocked(event{Kind: "permission_closed", ID: id})
		}
	}()

	select {
	case <-ctx.Done():
		return cancelled, nil
	case <-c.op.ctx.Done():
		return cancelled, nil
	case decision := <-pending.decision:
		if ctx.Err() != nil || c.op.ctx.Err() != nil {
			return cancelled, nil
		}
		option := deny
		if decision == "allow" {
			option = allow
		}
		if option == "" {
			return cancelled, nil
		}
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{Outcome: "selected", OptionId: option},
		}}, nil
	}
}

// Grok executes its tools. The client advertises no filesystem or terminal
// capability and also rejects these requests if a peer sends one anyway.
func (*acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (*acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (*acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}

func (*acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}

func (*acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}

func (*acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}

func (*acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}
