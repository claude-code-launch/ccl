package webui

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// EffortLevels are the values Claude Code's --effort accepts.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// LaunchSpec is everything one conversation turn needs.
type LaunchSpec struct {
	Provider string
	// Model is a slot alias (opus, sonnet, …) or a full model id.
	Model     string
	Effort    string
	Workspace string
	// Resume is the Claude session to continue; empty starts a new one.
	Resume string
	Prompt string
	// PermissionMode is Claude Code's --permission-mode. Every mode but
	// bypassPermissions asks the page before tools that need approval.
	PermissionMode string
	// Attachments are files sent with the prompt. A turn with attachments
	// writes its message to stdin as stream-json, which can carry images.
	Attachments []LaunchAttachment
	// attachmentViews is what the stored user message shows of them.
	attachmentViews []Attachment
}

// LaunchAttachment is one attached file as the process sees it.
type LaunchAttachment struct {
	Path      string
	Name      string
	MediaType string
	// Image sends the file as an image block; otherwise Claude Code gets its path.
	Image bool
}

// LaunchFunc prepares and starts the Claude Code process for one turn.
// Stdout and stderr are wired by the caller through cmd. It must not touch the
// caller's stdio. The returned cleanup runs when the process has exited.
type LaunchFunc func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error)

// Runner executes turns for a session and serializes them.
type Runner struct {
	// Prepare launches Claude Code. It is a field so tests can inject a fake.
	Prepare LaunchFunc
	// MaxConcurrency bounds how many runs execute at once across sessions.
	MaxConcurrency int

	sem chan struct{}

	mu sync.Mutex
	// latest is the most recent init event: what Claude Code offers.
	latest InitPayload
}

// Capabilities is the most recent init event any turn reported.
func (r *Runner) Capabilities() InitPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest
}

func (r *Runner) remember(init InitPayload) {
	r.mu.Lock()
	r.latest = init
	r.mu.Unlock()
}

// NewRunner returns a runner using the real ccl launcher.
func NewRunner(maxConcurrency int) *Runner {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	return &Runner{Prepare: DefaultLauncher, MaxConcurrency: maxConcurrency, sem: make(chan struct{}, maxConcurrency)}
}

// DefaultLauncher prepares Claude Code exactly as a ccl terminal session would,
// but with the provider taken from the conversation rather than the active
// selection. It never writes ~/.ccl/config.yaml.
func DefaultLauncher(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
	args := buildArgs(spec)
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load ccl config: %w", err)
	}
	if provider.IsDefaultProvider(spec.Provider) {
		claudePath, err := exec.LookPath("claude")
		if err != nil {
			return nil, nil, fmt.Errorf("claude CLI not found in PATH: %w", err)
		}
		cmd := process.Command(claudePath, args...)
		cmd.Env = os.Environ()
		return cmd, func() {}, nil
	}
	p, ok := cfg.Providers[spec.Provider]
	if !ok || strings.TrimSpace(spec.Provider) == "" {
		return nil, nil, fmt.Errorf("provider %q not found; use ccl ls or ccl set", spec.Provider)
	}
	p.Name = spec.Provider
	launch, err := claude.PrepareContext(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	return launch.Command(args...), launch.Close, nil
}

// buildArgs is the Claude Code command line for one turn. --verbose is
// required by --print with --output-format stream-json.
func buildArgs(spec LaunchSpec) []string {
	mode := strings.TrimSpace(spec.PermissionMode)
	if mode == "" {
		mode = "bypassPermissions"
	}
	// The message arrives on stdin as stream-json (see stdinMessage), which
	// stays open for the turn: permission answers travel on it too.
	args := []string{
		"--print",
		"--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", mode,
	}
	if mode != "bypassPermissions" {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if model := strings.TrimSpace(spec.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(spec.Effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if resume := strings.TrimSpace(spec.Resume); resume != "" {
		args = append(args, "--resume", resume)
	}
	return args
}

// stdinMessage is the stream-json user message of a turn: attached images as
// base64 image blocks first, then the text, which lists other attached files
// by path for Claude Code to read.
func stdinMessage(spec LaunchSpec) ([]byte, error) {
	type imageSource struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type block struct {
		Type   string       `json:"type"`
		Text   string       `json:"text,omitempty"`
		Source *imageSource `json:"source,omitempty"`
	}
	content := []block{}
	var files []string
	for _, attachment := range spec.Attachments {
		if !attachment.Image {
			files = append(files, fmt.Sprintf("- %s (%s)", attachment.Path, attachment.Name))
			continue
		}
		data, err := os.ReadFile(attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", attachment.Name, err)
		}
		content = append(content, block{Type: "image", Source: &imageSource{
			Type:      "base64",
			MediaType: attachment.MediaType,
			Data:      base64.StdEncoding.EncodeToString(data),
		}})
	}
	text := spec.Prompt
	if len(files) > 0 {
		text = strings.TrimSpace(text + "\n\nAttached files:\n" + strings.Join(files, "\n"))
	}
	if text != "" {
		content = append(content, block{Type: "text", Text: text})
	}
	message := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
	line, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// ErrBusy reports a turn already running for the session.
var ErrBusy = errors.New("a turn is already running")

// turn is one in-flight run.
type turn struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	// ended closes once the turn's result is stored: the process is only
	// exiting, so the next request waits for done instead of being busy.
	ended   chan struct{}
	endOnce sync.Once
	// abort wakes a turn still waiting for a global slot.
	abort chan struct{}
	// cancelRequested records a user stop so the exit is not reported as a crash.
	cancelRequested bool
	mu              sync.Mutex

	// stdin carries the message and permission answers; it closes when the
	// turn's result arrives, which ends the process.
	stdin   io.WriteCloser
	writeMu sync.Mutex
	closed  bool
	// pending holds permission requests awaiting the user, with the original
	// input that answers must send back.
	pending map[string]pendingPermission
}

type pendingPermission struct {
	toolName    string
	input       json.RawMessage
	suggestions json.RawMessage
}

// write sends one line to Claude Code's stdin unless the turn ended.
func (e *turn) write(line []byte) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.mu.Lock()
	stdin, closed := e.stdin, e.closed
	e.mu.Unlock()
	if stdin == nil || closed {
		return errTurnEnded
	}
	_, err := stdin.Write(line)
	return err
}

func (e *turn) closeStdin() {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.mu.Lock()
	stdin := e.stdin
	already := e.closed
	e.closed = true
	e.pending = nil
	e.mu.Unlock()
	if stdin != nil && !already {
		_ = stdin.Close()
	}
}

// TurnHandle is what a started turn exposes.
type TurnHandle struct {
	// Done closes when the process has exited and its events are stored.
	Done <-chan struct{}
	// Session is the session the turn appends to.
	Session *Session
	// Cancel asks the process to stop: SIGINT, then SIGKILL after 2s.
	Cancel func()
}

// Timeline is the per-session turn gate.
type Timeline struct {
	mu      sync.Mutex
	running map[string]*turn
}

// NewTimeline returns an empty turn gate.
func NewTimeline() *Timeline { return &Timeline{running: make(map[string]*turn)} }

// Running reports whether the session has a turn in flight.
// Settle waits up to timeout for a turn that already stored its result to
// exit, so a message sent right after the result is not refused as busy.
// A turn still working is not waited for.
func (t *Timeline) Settle(id string, timeout time.Duration) {
	t.mu.Lock()
	entry, ok := t.running[id]
	t.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-entry.ended:
	default:
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-entry.done:
	case <-timer.C:
	}
}

func (t *Timeline) Running(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.running[id]
	return ok
}

// CancelAll stops every turn in flight and returns channels that close once
// each one is stored.
func (t *Timeline) CancelAll() []<-chan struct{} {
	t.mu.Lock()
	entries := make([]*turn, 0, len(t.running))
	for _, entry := range t.running {
		entries = append(entries, entry)
	}
	t.mu.Unlock()
	done := make([]<-chan struct{}, 0, len(entries))
	for _, entry := range entries {
		t.cancel(entry)
		done = append(done, entry.done)
	}
	return done
}

// RequestCancel stops the turn in flight for a session, if any.
func (t *Timeline) RequestCancel(id string) bool {
	t.mu.Lock()
	entry, ok := t.running[id]
	t.mu.Unlock()
	if !ok {
		return false
	}
	t.cancel(entry)
	return true
}

// Start runs one turn: it stores the user message, launches Claude Code, and
// stores every translated event. It returns ErrBusy when a turn already runs.
//
// A turn that has to wait for a global slot returns at once and launches in
// the background; a launch failure is then reported in the history only.
func (t *Timeline) Start(r *Runner, session *Session, spec LaunchSpec) (*TurnHandle, error) {
	id := session.ID()

	t.mu.Lock()
	if _, busy := t.running[id]; busy {
		t.mu.Unlock()
		return nil, ErrBusy
	}
	entry := &turn{done: make(chan struct{}), ended: make(chan struct{}), abort: make(chan struct{})}
	t.running[id] = entry
	t.mu.Unlock()

	relinquish := func() {
		t.mu.Lock()
		delete(t.running, id)
		t.mu.Unlock()
		close(entry.done)
	}

	if _, err := session.Append(EventUser, UserPayload{Text: spec.Prompt, Attachments: spec.attachmentViews}); err != nil {
		relinquish()
		return nil, err
	}
	_, _ = session.UpdateMeta(func(meta *Meta) { meta.Turns++ })
	handle := &TurnHandle{
		Done:    entry.done,
		Session: session,
		Cancel:  func() { t.cancel(entry) },
	}

	// Only a turn that actually waits for a slot says so.
	select {
	case r.sem <- struct{}{}:
		if err := t.launch(r, session, entry, spec, relinquish); err != nil {
			return nil, err
		}
		return handle, nil
	default:
	}
	if _, err := session.Append(EventStatus, StatusPayload{State: "queued"}); err != nil {
		relinquish()
		return nil, err
	}
	go func() {
		select {
		case r.sem <- struct{}{}:
		case <-entry.abort:
			_, _ = session.Append(EventResult, ResultPayload{Cancelled: true, Message: "cancelled while queued"})
			_ = session.Touch()
			relinquish()
			return
		}
		// A failure is already stored in the conversation history.
		_ = t.launch(r, session, entry, spec, relinquish)
	}()
	return handle, nil
}

// launch starts the process for a turn that holds a global slot.
func (t *Timeline) launch(r *Runner, session *Session, entry *turn, spec LaunchSpec, relinquish func()) error {
	ctx, cancel := context.WithCancel(context.Background())
	entry.mu.Lock()
	entry.cancel = cancel
	stopped := entry.cancelRequested
	entry.mu.Unlock()

	fail := func(cleanup func(), err error) error {
		cancel()
		if cleanup != nil {
			cleanup()
		}
		<-r.sem
		_, _ = session.Append(EventError, ErrorPayload{Message: err.Error()})
		_, _ = session.Append(EventResult, ResultPayload{IsError: true})
		_ = session.Touch()
		relinquish()
		return err
	}

	// A stop that arrived while the turn waited for its slot wins.
	if stopped {
		cancel()
		<-r.sem
		_, _ = session.Append(EventResult, ResultPayload{Cancelled: true, Message: "cancelled before it started"})
		_ = session.Touch()
		relinquish()
		return nil
	}

	cmd, cleanup, err := r.Prepare(ctx, spec)
	if err != nil {
		return fail(nil, err)
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Claude stdout: %w", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Claude stderr: %w", err))
	}
	message, err := stdinMessage(spec)
	if err != nil {
		return fail(cleanup, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Claude stdin: %w", err))
	}
	cmd.Dir = spec.Workspace
	process.ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fail(cleanup, fmt.Errorf("start Claude Code: %w", err))
	}
	entry.mu.Lock()
	entry.cmd = cmd
	entry.stdin = stdin
	stopped = entry.cancelRequested
	entry.mu.Unlock()
	// The pipe is unbuffered; a large image must not hold up the request.
	go func() { _ = entry.write(message) }()
	// cancel() saw no process yet; it is this goroutine's to stop.
	if stopped {
		interrupt(cmd, entry.done)
	}

	go t.pump(r, session, entry, stdout, stderr, cleanup, cancel, relinquish)
	return nil
}

func (t *Timeline) cancel(entry *turn) {
	entry.mu.Lock()
	alreadyRequested := entry.cancelRequested
	entry.cancelRequested = true
	cmd := entry.cmd
	cancel := entry.cancel
	entry.mu.Unlock()
	if alreadyRequested {
		return
	}
	// A turn still waiting for a slot has no process to signal.
	close(entry.abort)
	if cancel != nil {
		cancel()
	}
	if cmd != nil {
		entry.closeStdin()
		interrupt(cmd, entry.done)
	}
}

// interrupt asks the process to stop: SIGINT, then SIGKILL of its tree after
// 2s unless done closes first.
func interrupt(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	go func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			process.KillTree(cmd)
		}
	}()
}

func (t *Timeline) pump(r *Runner, session *Session, entry *turn, stdout, stderr io.Reader, cleanup func(), cancel context.CancelFunc, relinquish func()) {
	defer relinquish()
	defer cancel()
	defer cleanup()
	defer func() { <-r.sem }()

	translator := newTranslator()
	// Reading and translating happen on the same goroutine as the process
	// output: ordering is what the browser renders.
	stderrText := make([]string, 0, 8)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderrText = readLines(stderr, 40, stderrText)
	}()

	cancelled := func() bool {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return entry.cancelRequested
	}
	// The browser treats a result as the end of the turn, so exactly one is
	// stored: Claude Code's own, or one written here when it reported none.
	sawResult := false

	readStream(stdout, func(line []byte) {
		translator.Handle(line, func(kind string, payload any) {
			if kind == EventInit {
				if init, ok := payload.(InitPayload); ok {
					r.remember(init)
				}
				if init, ok := payload.(InitPayload); ok && init.SessionID != "" {
					_, _ = session.UpdateMeta(func(meta *Meta) {
						if meta.ClaudeSessionID == "" {
							meta.ClaudeSessionID = init.SessionID
						}
					})
				}
			}
			if kind == EventPermission {
				if request, ok := payload.(PermissionPayload); ok {
					entry.mu.Lock()
					if entry.pending == nil {
						entry.pending = map[string]pendingPermission{}
					}
					entry.pending[request.RequestID] = pendingPermission{toolName: request.ToolName, input: request.raw, suggestions: request.suggestions}
					entry.mu.Unlock()
				}
			}
			if kind == EventPermissionResolved {
				if resolved, ok := payload.(PermissionResolvedPayload); ok {
					entry.mu.Lock()
					delete(entry.pending, resolved.RequestID)
					entry.mu.Unlock()
				}
			}
			if kind == eventUnsupportedControl {
				// Nothing here answers other requests; saying so keeps
				// Claude Code from waiting for an answer that never comes.
				if id, ok := payload.(string); ok {
					_ = entry.write(controlError(id, "ccl web does not handle this request"))
				}
				return
			}
			if kind == EventResult {
				// The turn is over: closing stdin lets the process exit.
				defer entry.closeStdin()
				if sawResult {
					return
				}
				sawResult = true
				if result, ok := payload.(ResultPayload); ok {
					_, _ = session.UpdateMeta(func(meta *Meta) {
						turn := meta.recordTurn(result.Usage)
						result.TurnUsage = &turn
					})
					result.Cancelled = cancelled()
					payload = result
				}
			}
			_, _ = session.Append(kind, payload)
			if kind == EventResult {
				entry.endOnce.Do(func() { close(entry.ended) })
			}
		})
	})

	// readLines drains to EOF (it discards past its limit), so joining it before
	// Wait cannot deadlock, and Wait is documented to close the pipes.
	<-stderrDone
	entry.closeStdin()
	waitErr := entry.cmd.Wait()
	stopped := cancelled()
	message := strings.TrimSpace(strings.Join(stderrText, "\n"))
	if waitErr != nil && !stopped && (message != "" || !sawResult) {
		if message == "" {
			message = waitErr.Error()
		}
		_, _ = session.Append(EventError, ErrorPayload{Message: truncateRunes(redactSecrets(message), 4000)})
	}
	if !sawResult {
		closing := ResultPayload{Cancelled: stopped}
		if !stopped {
			closing.IsError = true
			if waitErr == nil {
				closing.Message = "Claude Code exited without a result"
			}
		}
		_, _ = session.Append(EventResult, closing)
	}
	_ = session.Touch()
}

// readStream calls handle for every line of r until EOF.
func readStream(r io.Reader, handle func(line []byte)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	for scanner.Scan() {
		handle(scanner.Bytes())
	}
}

// readLines appends at most limit lines of r to out. Diagnostic output beyond
// that is not worth storing.
func readLines(r io.Reader, limit int, out []string) []string {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for scanner.Scan() {
		if len(out) >= limit {
			continue
		}
		if line := strings.TrimRight(scanner.Text(), "\r"); line != "" {
			out = append(out, line)
		}
	}
	return out
}
