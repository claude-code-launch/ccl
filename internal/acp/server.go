package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// LaunchLease owns one reference to a prepared Claude Code launch generation.
// Generation is stable for equivalent provider configuration. Release must be
// idempotent because session shutdown and prompt cancellation can overlap.
type LaunchLease interface {
	Generation() uint64
	Command(cwd string, extra []string) *exec.Cmd
	Release()
}

// Config is the ACP server's Claude Code launch spec.
type Config struct {
	ClaudePath   string
	SettingsPath string
	Env          []string
	AgentName    string
	AgentTitle   string
	AgentVersion string
	Stderr       io.Writer
	// NewCommand, if set, replaces the default Claude Code exec. Tests use this
	// to inject a fake process. extra holds ClaudeArgs plus mcp/permission/resume.
	NewCommand func(cwd string, extra []string) *exec.Cmd
	// AcquireLaunch enables provider refresh at prompt boundaries. Each returned
	// lease is owned by one ACP session until it is replaced or the session closes.
	// Static NewCommand behavior is retained when this callback is nil.
	AcquireLaunch func(context.Context) (LaunchLease, error)
	// SessionStorePath is a directory for ACP-to-Claude session mappings. An
	// empty path disables cross-process session/load persistence.
	SessionStorePath string
}

// Serve reads ACP JSON-RPC from in and writes responses and notifications to out.
func Serve(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.AgentName == "" {
		cfg.AgentName = "ccl"
	}
	if cfg.AgentTitle == "" {
		cfg.AgentTitle = "CCL"
	}
	s := &server{
		ctx:     ctx,
		cfg:     cfg,
		enc:     encoder{out: out, pending: map[string]chan rpcResponse{}},
		session: map[string]*claudeSession{},
		store:   newSessionStore(cfg.SessionStorePath),
	}
	s.store.prune(sessionStoreMaxAge, time.Now())
	defer func() {
		s.shutdown()
		s.wg.Wait()
	}()

	scanner := bufio.NewScanner(bindReader(ctx, in))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		copied := append([]byte(nil), line...)
		if err := s.dispatch(copied); err != nil {
			return err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

type server struct {
	ctx     context.Context
	cfg     Config
	enc     encoder
	wg      sync.WaitGroup
	mu      sync.Mutex
	ready   bool
	session map[string]*claudeSession
	store   *sessionStore
}

func (s *server) shutdown() {
	s.mu.Lock()
	sessions := make([]*claudeSession, 0, len(s.session))
	for id, sess := range s.session {
		sessions = append(sessions, sess)
		delete(s.session, id)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
}

func (s *server) dispatch(line []byte) error {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return s.enc.replyError(json.RawMessage("null"), errParse, "parse error")
	}
	if req.Method == "" {
		if !isNotification(req.ID) {
			s.enc.complete(req.ID, line)
		}
		return nil
	}
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized":
		return nil
	case "session/new":
		return s.handleSessionNew(req)
	case "session/load":
		return s.handleSessionLoad(req)
	case "session/prompt":
		run, err := s.beginPrompt(req)
		if err != nil || run == nil {
			return err
		}
		s.wg.Go(run)
		return nil
	case "session/cancel":
		return s.handleCancel(req)
	case "session/close":
		return s.handleSessionClose(req)
	default:
		if !s.initialized() && req.Method != "initialize" {
			return s.enc.replyError(req.ID, errInvalidRequest, "server not initialized")
		}
		return s.enc.replyError(req.ID, errMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *server) initialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *server) handleInitialize(req rpcRequest) error {
	var params initializeParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	logf(s.cfg.Stderr, "initialize")
	return s.enc.reply(req.ID, initializeResult{
		ProtocolVersion: protocolVersion,
		AgentCapabilities: agentCapabilities{
			LoadSession: true,
			PromptCapabilities: promptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			MCPCapabilities: mcpCapabilities{
				HTTP: true,
			},
			SessionCapabilities: sessionCapabilities{},
		},
		AgentInfo: agentInfo{
			Name:    s.cfg.AgentName,
			Title:   s.cfg.AgentTitle,
			Version: s.cfg.AgentVersion,
		},
		AuthMethods: []authMethod{},
	})
}

func (s *server) handleSessionNew(req rpcRequest) error {
	if !s.initialized() {
		return s.enc.replyError(req.ID, errInvalidRequest, "server not initialized")
	}
	var params newSessionParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.enc.replyError(req.ID, errInvalidParams, "invalid session/new params")
		}
	}
	cwd, err := resolveSessionCWD(params.CWD)
	if err != nil {
		return s.enc.replyError(req.ID, errInvalidParams, err.Error())
	}
	sess := s.newSession(uuid.NewString(), cwd, params.MCPServers)
	if s.cfg.AcquireLaunch != nil {
		launch, acquireErr := s.acquireLaunch(s.ctx)
		if acquireErr != nil {
			sess.close()
			return s.enc.replyError(req.ID, errInternal, acquireErr.Error())
		}
		sess.mu.Lock()
		sess.launch = launch
		sess.launchGeneration = launch.Generation()
		sess.mu.Unlock()
	}
	sess.mu.Lock()
	err = sess.ensureProcess()
	sess.mu.Unlock()
	if err != nil {
		sess.close()
		return s.enc.replyError(req.ID, errInternal, err.Error())
	}
	s.mu.Lock()
	s.session[sess.id] = sess
	s.mu.Unlock()
	logf(s.cfg.Stderr, "session/new id=%s cwd=%s mcp=%d", sess.id, cwd, len(params.MCPServers))
	return s.enc.reply(req.ID, newSessionResult{SessionID: sess.id})
}

func (s *server) handleSessionLoad(req rpcRequest) error {
	if !s.initialized() {
		return s.enc.replyError(req.ID, errInvalidRequest, "server not initialized")
	}
	var params loadSessionParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionID == "" {
		return s.enc.replyError(req.ID, errInvalidParams, "invalid session/load params")
	}
	cwd := params.CWD
	if cwd != "" {
		var err error
		cwd, err = resolveSessionCWD(cwd)
		if err != nil {
			return s.enc.replyError(req.ID, errInvalidParams, err.Error())
		}
	}
	sess := s.lookup(params.SessionID)
	newSession := false
	if sess == nil {
		entry, err := s.store.load(params.SessionID)
		if err != nil {
			if os.IsNotExist(err) {
				return s.enc.replyError(req.ID, errInvalidParams, "unknown sessionId")
			}
			return s.enc.replyError(req.ID, errInternal, err.Error())
		}
		if cwd == "" {
			cwd = entry.CWD
		}
		servers := params.MCPServers
		sess = s.newSession(params.SessionID, cwd, servers)
		sess.mu.Lock()
		sess.claudeID = entry.ClaudeSessionID
		sess.mu.Unlock()
		newSession = true
	}
	sess.mu.Lock()
	if sess.prompting {
		sess.mu.Unlock()
		if newSession {
			sess.close()
		}
		return s.enc.replyError(req.ID, errInternal, "session is busy")
	}
	needsLaunch := s.cfg.AcquireLaunch != nil && sess.launch == nil
	sess.mu.Unlock()

	var candidate LaunchLease
	var err error
	if needsLaunch {
		candidate, err = s.acquireLaunch(s.ctx)
		if err != nil {
			if newSession {
				sess.close()
			}
			return s.enc.replyError(req.ID, errInternal, err.Error())
		}
	}

	sess.mu.Lock()
	previousClosed := sess.closed
	previousCWD := sess.cwd
	previousServers := append([]mcpServer(nil), sess.mcpServers...)
	previousLaunch := sess.launch
	previousGeneration := sess.launchGeneration
	if candidate != nil && previousGeneration != 0 && previousGeneration != candidate.Generation() && sess.promptSent && sess.claudeID == "" {
		sess.mu.Unlock()
		candidate.Release()
		if newSession {
			sess.close()
		}
		return s.enc.replyError(req.ID, errInternal, "cannot switch the configuration used by ACP because the Claude session ID is not available; create a new session")
	}
	if cwd != "" || params.CWD != "" || newSession {
		if cwd != "" {
			sess.cwd = cwd
		}
	}
	if params.MCPServers != nil {
		sess.mcpServers = params.MCPServers
		if err = sess.rewriteMCPConfig(); err != nil {
			sess.cwd = previousCWD
			sess.mcpServers = previousServers
			sess.mu.Unlock()
			if candidate != nil {
				candidate.Release()
			}
			if newSession {
				sess.close()
			}
			return s.enc.replyError(req.ID, errInternal, err.Error())
		}
	}
	sess.killLocked()
	if candidate != nil {
		sess.launch = candidate
		sess.launchGeneration = candidate.Generation()
	}
	err = sess.ensureProcess()
	if err != nil {
		sess.closed = previousClosed
		sess.cwd = previousCWD
		sess.mcpServers = previousServers
		sess.launch = previousLaunch
		sess.launchGeneration = previousGeneration
	} else {
		sess.closed = false
	}
	sess.mu.Unlock()
	if err != nil {
		if candidate != nil {
			candidate.Release()
		}
		if newSession {
			sess.close()
		}
		return s.enc.replyError(req.ID, errInternal, err.Error())
	}
	if newSession {
		s.mu.Lock()
		s.session[sess.id] = sess
		s.mu.Unlock()
	}
	s.persistSession(sess)
	logf(s.cfg.Stderr, "session/load id=%s", sess.id)
	sess.replayHistory(func(update any) {
		_ = s.enc.notify("session/update", sessionUpdateParams{
			SessionID: sess.id,
			Update:    update,
		})
	})
	return s.enc.reply(req.ID, map[string]any{})
}

func (s *server) acquireLaunch(ctx context.Context) (LaunchLease, error) {
	if s.cfg.AcquireLaunch == nil {
		return nil, fmt.Errorf("dynamic Claude launch is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	launch, err := s.cfg.AcquireLaunch(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire Claude launch: %w", err)
	}
	if launch == nil {
		return nil, fmt.Errorf("acquire Claude launch returned nil")
	}
	if err := ctx.Err(); err != nil {
		launch.Release()
		return nil, err
	}
	if launch.Generation() == 0 {
		launch.Release()
		return nil, fmt.Errorf("acquire Claude launch returned an invalid generation")
	}
	return launch, nil
}

func (s *server) newSession(id, cwd string, servers []mcpServer) *claudeSession {
	sess := &claudeSession{
		id:           id,
		cwd:          cwd,
		cfg:          s.cfg,
		stderr:       s.cfg.Stderr,
		mcpServers:   servers,
		allowAlways:  map[string]bool{},
		rejectAlways: map[string]bool{},
		persist:      s.persistSession,
	}
	sess.askPermission = func(ctx context.Context, req permSocketRequest) permSocketReply {
		return s.requestPermission(ctx, sess, req)
	}
	return sess
}

func (s *server) persistSession(sess *claudeSession) {
	if s.store == nil {
		return
	}
	sess.mu.Lock()
	entry := persistedSession{
		SessionID:       sess.id,
		ClaudeSessionID: sess.claudeID,
		CWD:             sess.cwd,
	}
	sess.mu.Unlock()
	if err := s.store.save(entry); err != nil {
		logf(s.cfg.Stderr, "persist ACP session %s: %v", entry.SessionID, err)
	}
}

func (s *server) requestPermission(ctx context.Context, sess *claudeSession, req permSocketRequest) permSocketReply {
	deny := permSocketReply{Behavior: "deny", Message: "permission denied"}
	id := req.ToolUseID
	if id == "" {
		id = uuid.NewString()
	}
	logf(s.cfg.Stderr, "session/request_permission session=%s tool=%s", sess.id, req.ToolName)
	params := requestPermissionParams{
		SessionID: sess.id,
		ToolCall: toolCallUpdate{
			ToolCallID: id,
			Title:      toolTitle(req.ToolName, req.Input),
			Name:       req.ToolName,
			Kind:       toolKind(req.ToolName),
			Status:     "pending",
			Locations:  toolLocations(sess.cwd, req.ToolName, req.Input),
			Content:    toolDiffs(sess.cwd, req.ToolName, req.Input),
			RawInput:   rawAsAny(req.Input),
		},
		Options: defaultPermissionOptions,
	}
	raw, err := s.enc.request(ctx, "session/request_permission", params)
	if err != nil {
		return deny
	}
	var result permissionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return deny
	}
	if result.Outcome.Outcome == "cancelled" {
		return permSocketReply{Behavior: "deny", Message: "cancelled"}
	}
	if result.Outcome.Outcome != "selected" {
		return deny
	}
	allow := permSocketReply{Behavior: "allow", UpdatedInput: req.Input}
	if len(allow.UpdatedInput) == 0 {
		allow.UpdatedInput = json.RawMessage(`{}`)
	}
	switch result.Outcome.OptionID {
	case "allow-once":
		return allow
	case "allow-always":
		sess.mu.Lock()
		sess.allowAlways[req.ToolName] = true
		sess.mu.Unlock()
		return allow
	case "reject-always":
		sess.mu.Lock()
		sess.rejectAlways[req.ToolName] = true
		sess.mu.Unlock()
		return deny
	default:
		return deny
	}
}

// beginPrompt validates and books the session slot on the read loop so a
// following session/cancel cannot miss promptCancel.
func (s *server) beginPrompt(req rpcRequest) (func(), error) {
	if !s.initialized() {
		return nil, s.enc.replyError(req.ID, errInvalidRequest, "server not initialized")
	}
	var params promptParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionID == "" {
		return nil, s.enc.replyError(req.ID, errInvalidParams, "invalid session/prompt params")
	}
	sess := s.lookup(params.SessionID)
	if sess == nil {
		return nil, s.enc.replyError(req.ID, errInvalidParams, "unknown sessionId")
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if closed {
		return nil, s.enc.replyError(req.ID, errInvalidParams, "unknown sessionId")
	}
	content := promptToClaudeContent(sess.cwd, params.Prompt, s.cfg.Stderr)
	if len(content) == 0 {
		return nil, s.enc.replyError(req.ID, errInvalidParams, "prompt has no supported content")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	sess.mu.Lock()
	if sess.prompting {
		sess.mu.Unlock()
		cancel()
		return nil, s.enc.replyError(req.ID, errInternal, "session is busy")
	}
	sess.prompting = true
	sess.promptCancel = cancel
	sess.promptCtx = ctx
	sess.mu.Unlock()
	logf(s.cfg.Stderr, "session/prompt session=%s blocks=%d", sess.id, len(content))

	return func() {
		var finishOnce sync.Once
		finish := func() {
			finishOnce.Do(func() {
				cancel()
				sess.mu.Lock()
				sess.prompting = false
				sess.promptCancel = nil
				sess.promptCtx = nil
				sess.mu.Unlock()
			})
		}
		defer finish()
		reason, err := sess.runPrompt(ctx, content, func(update any) {
			_ = s.enc.notify("session/update", sessionUpdateParams{
				SessionID: sess.id,
				Update:    update,
			})
		})
		// Make the prompt boundary visible before its response. A client may send
		// the next prompt as soon as it receives this reply. Capture cancellation
		// first because finish also releases the prompt context.
		cancelled := ctx.Err() != nil
		finish()
		if err != nil {
			if cancelled {
				_ = s.enc.reply(req.ID, promptResult{StopReason: stopCancelled})
				return
			}
			_ = s.enc.replyError(req.ID, errInternal, err.Error())
			return
		}
		_ = s.enc.reply(req.ID, promptResult{StopReason: reason})
	}, nil
}

func (s *server) handleCancel(req rpcRequest) error {
	var params cancelParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.enc.replyError(req.ID, errInvalidParams, "invalid session/cancel params")
		}
	}
	if params.SessionID == "" {
		return s.enc.replyError(req.ID, errInvalidParams, "missing sessionId")
	}
	sess := s.lookup(params.SessionID)
	if sess == nil {
		return s.enc.replyError(req.ID, errInvalidParams, "unknown sessionId")
	}
	sess.mu.Lock()
	cancel := sess.promptCancel
	sess.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	sess.mu.Lock()
	sess.killLocked()
	sess.mu.Unlock()
	logf(s.cfg.Stderr, "session/cancel session=%s", params.SessionID)
	return s.enc.reply(req.ID, map[string]any{})
}

func (s *server) handleSessionClose(req rpcRequest) error {
	if !s.initialized() {
		return s.enc.replyError(req.ID, errInvalidRequest, "server not initialized")
	}
	var params closeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.enc.replyError(req.ID, errInvalidParams, "invalid session/close params")
		}
	}
	if params.SessionID == "" {
		return s.enc.replyError(req.ID, errInvalidParams, "missing sessionId")
	}
	sess := s.lookup(params.SessionID)
	if sess == nil {
		return s.enc.replyError(req.ID, errInvalidParams, "unknown sessionId")
	}
	sess.close()
	logf(s.cfg.Stderr, "session/close session=%s", params.SessionID)
	return s.enc.reply(req.ID, map[string]any{})
}

func (s *server) lookup(id string) *claudeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session[id]
}

func resolveSessionCWD(cwd string) (string, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be an absolute path")
	}
	return cwd, nil
}

// bindReader stops Scan when ctx is cancelled. Background has a nil Done and
// is left unwrapped so a never-firing watcher is not leaked.
func bindReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx.Done() == nil {
		return r
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, r)
		if err == nil {
			err = io.EOF
		}
		_ = pw.CloseWithError(err)
	}()
	go func() {
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()
	return pr
}
