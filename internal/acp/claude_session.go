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
	"path/filepath"
	"strings"
	"sync"
)

const (
	stopEndTurn         = "end_turn"
	stopMaxTokens       = "max_tokens"
	stopMaxTurnRequests = "max_turn_requests"
	stopCancelled       = "cancelled"
)

var claudeACPArgs = []string{
	"--print",
	"--verbose",
	"--output-format", "stream-json",
	"--input-format", "stream-json",
	"--strict-mcp-config",
}

// ClaudeArgs returns the Claude Code flags used for every ACP session.
func ClaudeArgs() []string {
	out := make([]string, len(claudeACPArgs))
	copy(out, claudeACPArgs)
	return out
}

type claudeUserMessage struct {
	Type    string            `json:"type"`
	Message claudeUserPayload `json:"message"`
}

type claudeUserPayload struct {
	Role    string          `json:"role"`
	Content []claudeContent `json:"content"`
}

type claudeStreamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	Message   struct {
		Role    string          `json:"role"`
		Content []claudeContent `json:"content"`
	} `json:"message"`
}

type claudeSession struct {
	id         string
	cwd        string
	cfg        Config
	stderr     io.Writer
	mcpServers []mcpServer

	askPermission func(ctx context.Context, req permSocketRequest) permSocketReply
	persist       func(*claudeSession)

	mu               sync.Mutex
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	stdout           io.ReadCloser
	launch           LaunchLease
	launchGeneration uint64
	promptSent       bool
	prompting        bool
	promptCancel     context.CancelFunc
	promptCtx        context.Context
	closed           bool
	claudeID         string
	workDir          string
	mcpConfig        string
	permSock         string
	permLn           net.Listener
	allowAlways      map[string]bool
	rejectAlways     map[string]bool
}

func (s *claudeSession) close() {
	s.mu.Lock()
	if s.promptCancel != nil {
		s.promptCancel()
		s.promptCancel = nil
	}
	s.killLocked()
	launch := s.launch
	s.launch = nil
	ln := s.permLn
	s.permLn = nil
	dir := s.workDir
	s.workDir = ""
	s.mcpConfig = ""
	s.permSock = ""
	s.closed = true
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	if launch != nil {
		launch.Release()
	}
}

func (s *claudeSession) killLocked() {
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	cmd := s.cmd
	s.cmd = nil
	s.stdout = nil
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
	}
}

func (s *claudeSession) launchArgs() []string {
	args := ClaudeArgs()
	if s.mcpConfig != "" {
		args = append(args, "--mcp-config", s.mcpConfig, "--permission-prompt-tool", "mcp__cclperm__approve")
	}
	if s.claudeID != "" {
		args = append(args, "--resume", s.claudeID)
	}
	return args
}

func (s *claudeSession) ensurePermissionBridge() error {
	if s.permLn != nil && s.mcpConfig != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "ccl-acp-")
	if err != nil {
		return fmt.Errorf("permission workspace: %w", err)
	}
	sock := filepath.Join(dir, "perm.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("permission socket: %w", err)
	}
	cfgPath := filepath.Join(dir, "mcp.json")
	if err := writeMCPConfig(cfgPath, cclExecutable(), sock, s.mcpServers, s.stderr); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return fmt.Errorf("mcp-config: %w", err)
	}
	s.workDir = dir
	s.mcpConfig = cfgPath
	s.permSock = sock
	s.permLn = ln
	go s.acceptPermission(ln)
	return nil
}

func (s *claudeSession) rewriteMCPConfig() error {
	if s.mcpConfig == "" || s.permSock == "" {
		return nil
	}
	return writeMCPConfig(s.mcpConfig, cclExecutable(), s.permSock, s.mcpServers, s.stderr)
}

func (s *claudeSession) acceptPermission(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handlePermConn(conn)
	}
}

func (s *claudeSession) handlePermConn(conn net.Conn) {
	defer conn.Close()
	var req permSocketRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	reply := s.decidePermission(req)
	_ = json.NewEncoder(conn).Encode(reply)
}

func (s *claudeSession) decidePermission(req permSocketRequest) permSocketReply {
	deny := permSocketReply{Behavior: "deny", Message: "permission denied"}
	allow := permSocketReply{Behavior: "allow", UpdatedInput: req.Input}
	if len(allow.UpdatedInput) == 0 {
		allow.UpdatedInput = json.RawMessage(`{}`)
	}

	s.mu.Lock()
	if s.rejectAlways[req.ToolName] {
		s.mu.Unlock()
		return permSocketReply{Behavior: "deny", Message: "rejected for this session"}
	}
	if s.allowAlways[req.ToolName] {
		s.mu.Unlock()
		return allow
	}
	ctx := s.promptCtx
	ask := s.askPermission
	s.mu.Unlock()
	if ask == nil {
		return deny
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return ask(ctx, req)
}

func (s *claudeSession) ensureProcess() error {
	if s.cmd != nil && s.cmd.Process != nil {
		return nil
	}
	s.killLocked()
	if err := s.ensurePermissionBridge(); err != nil {
		return err
	}
	var cmd *exec.Cmd
	if s.launch != nil {
		cmd = s.launch.Command(s.cwd, s.launchArgs())
	} else {
		if s.cfg.AcquireLaunch != nil {
			return fmt.Errorf("Claude launch has not been acquired")
		}
		cmd = s.cfg.command(s.cwd, s.launchArgs())
	}
	stdin, stdout, err := startClaudeProcess(cmd, s.stderr)
	if err != nil {
		return err
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	return nil
}

func startClaudeProcess(cmd *exec.Cmd, stderr io.Writer) (io.WriteCloser, io.ReadCloser, error) {
	if cmd == nil {
		return nil, nil, fmt.Errorf("Claude command is nil")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("claude stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("claude stdout pipe: %w", err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("start claude: %w", err)
	}
	return stdin, stdout, nil
}

func stopClaudeProcess(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) {
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
	}
}

func (s *claudeSession) refreshLaunch(ctx context.Context) error {
	acquire := s.cfg.AcquireLaunch
	if acquire == nil {
		return nil
	}
	candidate, err := acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire Claude launch: %w", err)
	}
	if candidate == nil {
		return fmt.Errorf("acquire Claude launch returned nil")
	}
	generation := candidate.Generation()
	if generation == 0 {
		candidate.Release()
		return fmt.Errorf("acquire Claude launch returned an invalid generation")
	}

	s.mu.Lock()
	if ctx.Err() != nil || s.closed || !s.prompting {
		s.mu.Unlock()
		candidate.Release()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ACP session is not available")
	}
	if s.launch != nil && s.launchGeneration == generation {
		s.mu.Unlock()
		candidate.Release()
		return nil
	}
	if s.launchGeneration != 0 && s.launchGeneration != generation && s.promptSent && s.claudeID == "" {
		s.mu.Unlock()
		candidate.Release()
		return fmt.Errorf("cannot switch the configuration used by ACP because the Claude session ID is not available; create a new session")
	}
	if err := s.ensurePermissionBridge(); err != nil {
		s.mu.Unlock()
		candidate.Release()
		return err
	}
	cmd := candidate.Command(s.cwd, s.launchArgs())
	stdin, stdout, err := startClaudeProcess(cmd, s.stderr)
	if err != nil {
		s.mu.Unlock()
		candidate.Release()
		return err
	}
	if ctx.Err() != nil {
		stopClaudeProcess(cmd, stdin, stdout)
		s.mu.Unlock()
		candidate.Release()
		return ctx.Err()
	}

	previous := s.launch
	s.killLocked()
	s.launch = candidate
	s.launchGeneration = generation
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.mu.Unlock()
	if previous != nil {
		previous.Release()
	}
	return nil
}

func (s *claudeSession) runPrompt(ctx context.Context, content []claudeContent, emit func(any)) (string, error) {
	if err := s.refreshLaunch(ctx); err != nil {
		if ctx.Err() != nil {
			return stopCancelled, nil
		}
		return "", err
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return stopCancelled, nil
	}
	if err := s.ensureProcess(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	stdin := s.stdin
	stdout := s.stdout
	s.mu.Unlock()
	if ctx.Err() != nil {
		s.mu.Lock()
		s.killLocked()
		s.mu.Unlock()
		return stopCancelled, nil
	}

	payload := claudeUserMessage{
		Type: "user",
		Message: claudeUserPayload{
			Role:    "user",
			Content: content,
		},
	}
	enc := json.NewEncoder(stdin)
	if err := enc.Encode(payload); err != nil {
		if ctx.Err() != nil {
			return stopCancelled, nil
		}
		s.mu.Lock()
		if s.stdin == stdin {
			s.killLocked()
		}
		s.mu.Unlock()
		return "", fmt.Errorf("write claude prompt: %w", err)
	}
	s.mu.Lock()
	s.promptSent = true
	s.mu.Unlock()

	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.killLocked()
			s.mu.Unlock()
		case <-watchDone:
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			s.mu.Lock()
			s.killLocked()
			s.mu.Unlock()
			return stopCancelled, nil
		}
		line := scanner.Bytes()
		var event claudeStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.SessionID != "" {
			s.mu.Lock()
			changed := s.claudeID != event.SessionID
			s.claudeID = event.SessionID
			persist := s.persist
			s.mu.Unlock()
			if changed && persist != nil {
				persist(s)
			}
		}
		switch event.Type {
		case "assistant", "user":
			emitClaudeEvent(event, s.cwd, emit, false)
		case "result":
			if ctx.Err() != nil {
				return stopCancelled, nil
			}
			return mapStopReason(event), nil
		}
	}
	if ctx.Err() != nil {
		s.mu.Lock()
		s.killLocked()
		s.mu.Unlock()
		return stopCancelled, nil
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return stopCancelled, nil
		}
		s.mu.Lock()
		s.killLocked()
		s.mu.Unlock()
		return "", fmt.Errorf("read claude stream: %w", err)
	}
	s.mu.Lock()
	s.killLocked()
	s.mu.Unlock()
	return stopEndTurn, nil
}

func (s *claudeSession) replayHistory(emit func(any)) {
	s.mu.Lock()
	id := s.claudeID
	cwd := s.cwd
	s.mu.Unlock()
	path := claudeTranscriptPath(cwd, id)
	if path == "" {
		return
	}
	if err := replayTranscript(path, cwd, emit); err != nil {
		if !os.IsNotExist(err) {
			logf(s.stderr, "transcript replay failed: %v", err)
		} else {
			logf(s.stderr, "no transcript at %s; chat history may be empty", path)
		}
	}
}

func mapStopReason(event claudeStreamEvent) string {
	switch {
	case event.Subtype == "error_max_turns":
		return stopMaxTurnRequests
	case event.Subtype == "error_max_output_tokens" || strings.Contains(event.Subtype, "max_tokens"):
		return stopMaxTokens
	default:
		return stopEndTurn
	}
}

func (c Config) command(cwd string, extra []string) *exec.Cmd {
	if c.NewCommand != nil {
		return c.NewCommand(cwd, extra)
	}
	args := make([]string, 0, 2+len(extra))
	if c.SettingsPath != "" {
		args = append(args, "--settings", c.SettingsPath)
	}
	args = append(args, extra...)
	cmd := exec.Command(c.ClaudePath, args...)
	cmd.Dir = cwd
	if len(c.Env) > 0 {
		cmd.Env = c.Env
	}
	return cmd
}
