package acp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type dynamicLaunchStart struct {
	generation uint64
	cwd        string
	args       []string
}

type dynamicLaunchSource struct {
	mu sync.Mutex

	executable  string
	generation  uint64
	mode        string
	capturePath string
	releasePath string
	acquireErr  error
	failStart   bool

	leases []*dynamicLaunchLease
	starts []dynamicLaunchStart
}

type dynamicLaunchLease struct {
	source      *dynamicLaunchSource
	generation  uint64
	mode        string
	capturePath string
	releasePath string
	failStart   bool
	once        sync.Once
	releases    atomic.Int32
}

func newDynamicLaunchSource(t *testing.T, generation uint64, mode string) *dynamicLaunchSource {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &dynamicLaunchSource{
		executable: executable,
		generation: generation,
		mode:       mode,
	}
}

func (s *dynamicLaunchSource) acquire(ctx context.Context) (LaunchLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	lease := &dynamicLaunchLease{
		source:      s,
		generation:  s.generation,
		mode:        s.mode,
		capturePath: s.capturePath,
		releasePath: s.releasePath,
		failStart:   s.failStart,
	}
	s.leases = append(s.leases, lease)
	return lease, nil
}

func (s *dynamicLaunchSource) set(generation uint64, mode string) {
	s.mu.Lock()
	s.generation = generation
	s.mode = mode
	s.acquireErr = nil
	s.failStart = false
	s.capturePath = ""
	s.releasePath = ""
	s.mu.Unlock()
}

func (s *dynamicLaunchSource) setAcquireFailure(generation uint64, err error) {
	s.mu.Lock()
	s.generation = generation
	s.acquireErr = err
	s.failStart = false
	s.mu.Unlock()
}

func (s *dynamicLaunchSource) setStartFailure(generation uint64) {
	s.mu.Lock()
	s.generation = generation
	s.acquireErr = nil
	s.failStart = true
	s.mu.Unlock()
}

func (s *dynamicLaunchSource) setWait(capturePath, releasePath string) {
	s.mu.Lock()
	s.mode = "wait"
	s.capturePath = capturePath
	s.releasePath = releasePath
	s.mu.Unlock()
}

func (s *dynamicLaunchSource) snapshot() ([]*dynamicLaunchLease, []dynamicLaunchStart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	leases := append([]*dynamicLaunchLease(nil), s.leases...)
	starts := make([]dynamicLaunchStart, len(s.starts))
	for i, start := range s.starts {
		starts[i] = dynamicLaunchStart{
			generation: start.generation,
			cwd:        start.cwd,
			args:       append([]string(nil), start.args...),
		}
	}
	return leases, starts
}

func (l *dynamicLaunchLease) Generation() uint64 {
	return l.generation
}

func (l *dynamicLaunchLease) Command(cwd string, extra []string) *exec.Cmd {
	l.source.mu.Lock()
	l.source.starts = append(l.source.starts, dynamicLaunchStart{
		generation: l.generation,
		cwd:        cwd,
		args:       append([]string(nil), extra...),
	})
	l.source.mu.Unlock()
	if l.failStart {
		return exec.Command("/no-such-ccl-acp-dynamic-claude")
	}
	cmd := exec.Command(l.source.executable, extra...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"CCL_ACP_FAKE_CLAUDE=1",
		"CCL_ACP_FAKE_MODE="+l.mode,
		"CCL_ACP_FAKE_CAPTURE="+l.capturePath,
		"CCL_ACP_FAKE_RELEASE="+l.releasePath,
	)
	return cmd
}

func (l *dynamicLaunchLease) Release() {
	l.once.Do(func() {
		l.releases.Add(1)
	})
}

func initializeDynamicACP(t *testing.T, source *dynamicLaunchSource, store string) *acpConn {
	t.Helper()
	c := startACP(t, Config{
		AcquireLaunch:    source.acquire,
		SessionStorePath: store,
	})
	initialized := c.call(t, 1, "initialize", map[string]any{"protocolVersion": 1})
	if initialized["error"] != nil {
		t.Fatalf("initialize: %v", initialized)
	}
	return c
}

func newDynamicSession(t *testing.T, c *acpConn, id int, cwd string) string {
	t.Helper()
	created := c.call(t, id, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if created["error"] != nil {
		t.Fatalf("session/new: %v", created)
	}
	result, _ := created["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("missing sessionId: %v", created)
	}
	return sid
}

func sendDynamicPrompt(t *testing.T, c *acpConn, id int, sid, text string) {
	t.Helper()
	if err := c.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": text}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func readDynamicPrompt(t *testing.T, c *acpConn, id int) map[string]any {
	t.Helper()
	for {
		msg := c.readRPC(t)
		if msg["method"] == "session/update" {
			continue
		}
		if msg["id"] != float64(id) {
			t.Fatalf("unexpected RPC while waiting for prompt %d: %v", id, msg)
		}
		return msg
	}
}

func dynamicPrompt(t *testing.T, c *acpConn, id int, sid, text string) map[string]any {
	t.Helper()
	sendDynamicPrompt(t, c, id, sid, text)
	return readDynamicPrompt(t, c, id)
}

func requirePromptSuccess(t *testing.T, msg map[string]any) {
	t.Helper()
	if msg["error"] != nil {
		t.Fatalf("prompt failed: %v", msg)
	}
	result, _ := msg["result"].(map[string]any)
	if result["stopReason"] != stopEndTurn {
		t.Fatalf("stopReason = %v", result)
	}
}

func rpcErrorMessage(msg map[string]any) string {
	errObject, _ := msg["error"].(map[string]any)
	message, _ := errObject["message"].(string)
	return message
}

func waitForDynamicCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timeout waiting for dynamic launch condition")
}

func TestDynamicLaunchReusesAndReplacesAtPromptBoundary(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())

	leases, starts := source.snapshot()
	if len(leases) != 1 || len(starts) != 1 || starts[0].generation != 1 {
		t.Fatalf("initial launch: leases=%d starts=%v", len(leases), starts)
	}
	if leases[0].releases.Load() != 0 {
		t.Fatal("active initial lease was released")
	}

	requirePromptSuccess(t, dynamicPrompt(t, c, 3, sid, "first"))
	requirePromptSuccess(t, dynamicPrompt(t, c, 4, sid, "second"))
	leases, starts = source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("unchanged generation restarted Claude: %v", starts)
	}
	if len(leases) != 3 {
		t.Fatalf("acquire count = %d, want 3", len(leases))
	}
	if leases[0].releases.Load() != 0 || leases[1].releases.Load() != 1 || leases[2].releases.Load() != 1 {
		t.Fatalf("same-generation release counts = %d/%d/%d", leases[0].releases.Load(), leases[1].releases.Load(), leases[2].releases.Load())
	}

	source.set(2, "")
	requirePromptSuccess(t, dynamicPrompt(t, c, 5, sid, "after switch"))
	leases, starts = source.snapshot()
	if len(starts) != 2 || starts[1].generation != 2 {
		t.Fatalf("replacement starts = %v", starts)
	}
	if got := flagValue(starts[1].args, "--resume"); got != "claude-sess-1" {
		t.Fatalf("replacement resume = %q, args=%v", got, starts[1].args)
	}
	for i, lease := range leases[:3] {
		if got := lease.releases.Load(); got != 1 {
			t.Fatalf("generation 1 lease %d release count = %d", i, got)
		}
	}
	if leases[3].releases.Load() != 0 {
		t.Fatal("active replacement lease was released early")
	}

	closed := c.call(t, 6, "session/close", map[string]any{"sessionId": sid})
	if closed["error"] != nil {
		t.Fatalf("session/close: %v", closed)
	}
	if got := leases[3].releases.Load(); got != 1 {
		t.Fatalf("replacement release count = %d, want 1", got)
	}
	_ = c.call(t, 7, "session/close", map[string]any{"sessionId": sid})
	if got := leases[3].releases.Load(); got != 1 {
		t.Fatalf("repeated close released replacement %d times", got)
	}
}

func TestDynamicLaunchChangeDuringPromptWaitsForNextPrompt(t *testing.T) {
	dir := t.TempDir()
	capturePath := dir + "/capture.json"
	releasePath := dir + "/release"
	source := newDynamicLaunchSource(t, 1, "wait")
	source.setWait(capturePath, releasePath)
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, dir)

	sendDynamicPrompt(t, c, 3, sid, "in flight")
	waitForDynamicCondition(t, func() bool {
		_, err := os.Stat(capturePath)
		return err == nil
	})
	source.set(2, "")
	_, starts := source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("provider change interrupted in-flight prompt: %v", starts)
	}
	if err := os.WriteFile(releasePath, []byte("continue"), 0o600); err != nil {
		t.Fatal(err)
	}
	requirePromptSuccess(t, readDynamicPrompt(t, c, 3))
	_, starts = source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("provider changed before prompt boundary: %v", starts)
	}

	requirePromptSuccess(t, dynamicPrompt(t, c, 4, sid, "next"))
	_, starts = source.snapshot()
	if len(starts) != 2 || starts[1].generation != 2 {
		t.Fatalf("next prompt did not switch generation: %v", starts)
	}
	if got := flagValue(starts[1].args, "--resume"); got != "claude-sess-1" {
		t.Fatalf("replacement resume = %q, args=%v", got, starts[1].args)
	}
}

func TestDynamicLaunchAcquireFailureKeepsCurrentProcess(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())
	requirePromptSuccess(t, dynamicPrompt(t, c, 3, sid, "first"))

	source.setAcquireFailure(2, errors.New("provider unavailable"))
	failed := dynamicPrompt(t, c, 4, sid, "failed switch")
	if rpcErrorCode(failed) != errInternal || !strings.Contains(rpcErrorMessage(failed), "provider unavailable") {
		t.Fatalf("acquire failure = %v", failed)
	}
	leases, starts := source.snapshot()
	if len(starts) != 1 || leases[0].releases.Load() != 0 {
		t.Fatalf("acquire failure changed active launch: starts=%v release=%d", starts, leases[0].releases.Load())
	}

	source.set(1, "")
	requirePromptSuccess(t, dynamicPrompt(t, c, 5, sid, "old process still works"))
	_, starts = source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("old process was restarted after acquire failure: %v", starts)
	}

	source.set(2, "")
	requirePromptSuccess(t, dynamicPrompt(t, c, 6, sid, "retry switch"))
	_, starts = source.snapshot()
	if len(starts) != 2 || starts[1].generation != 2 {
		t.Fatalf("retry did not switch: %v", starts)
	}
}

func TestDynamicLaunchStartFailureKeepsCurrentProcess(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())
	requirePromptSuccess(t, dynamicPrompt(t, c, 3, sid, "first"))

	source.setStartFailure(2)
	failed := dynamicPrompt(t, c, 4, sid, "failed start")
	if rpcErrorCode(failed) != errInternal || !strings.Contains(rpcErrorMessage(failed), "start claude") {
		t.Fatalf("start failure = %v", failed)
	}
	leases, starts := source.snapshot()
	if len(starts) != 2 || starts[1].generation != 2 {
		t.Fatalf("failed candidate was not attempted: %v", starts)
	}
	if leases[len(leases)-1].releases.Load() != 1 || leases[0].releases.Load() != 0 {
		t.Fatalf("failed candidate/active releases = %d/%d", leases[len(leases)-1].releases.Load(), leases[0].releases.Load())
	}

	source.set(1, "")
	requirePromptSuccess(t, dynamicPrompt(t, c, 5, sid, "old process still works"))
	_, starts = source.snapshot()
	if len(starts) != 2 {
		t.Fatalf("old process was lost after candidate start failure: %v", starts)
	}

	source.set(2, "")
	requirePromptSuccess(t, dynamicPrompt(t, c, 6, sid, "retry switch"))
	_, starts = source.snapshot()
	if len(starts) != 3 || starts[2].generation != 2 {
		t.Fatalf("retry did not start replacement: %v", starts)
	}
}

func TestDynamicLaunchRejectsSwitchWithoutClaudeSessionID(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "no_session")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())
	requirePromptSuccess(t, dynamicPrompt(t, c, 3, sid, "first"))

	source.set(2, "")
	failed := dynamicPrompt(t, c, 4, sid, "unsafe switch")
	if rpcErrorCode(failed) != errInternal || !strings.Contains(rpcErrorMessage(failed), "create a new session") {
		t.Fatalf("unsafe switch = %v", failed)
	}
	leases, starts := source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("unsafe replacement process started: %v", starts)
	}
	if leases[len(leases)-1].releases.Load() != 1 || leases[0].releases.Load() != 0 {
		t.Fatalf("candidate/active releases = %d/%d", leases[len(leases)-1].releases.Load(), leases[0].releases.Load())
	}

	source.set(1, "no_session")
	requirePromptSuccess(t, dynamicPrompt(t, c, 5, sid, "continue old session"))
	_, starts = source.snapshot()
	if len(starts) != 1 {
		t.Fatalf("old process was not preserved: %v", starts)
	}
}

func TestDynamicLaunchServerShutdownAndMultipleSessionsReleaseOnce(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "")
	c := initializeDynamicACP(t, source, "")
	firstID := newDynamicSession(t, c, 2, t.TempDir())
	_ = newDynamicSession(t, c, 3, t.TempDir())
	leases, _ := source.snapshot()
	if len(leases) != 2 {
		t.Fatalf("lease count = %d, want 2", len(leases))
	}

	closed := c.call(t, 4, "session/close", map[string]any{"sessionId": firstID})
	if closed["error"] != nil {
		t.Fatalf("session/close: %v", closed)
	}
	if leases[0].releases.Load() != 1 || leases[1].releases.Load() != 0 {
		t.Fatalf("close release counts = %d/%d", leases[0].releases.Load(), leases[1].releases.Load())
	}
	c.stop()
	if leases[0].releases.Load() != 1 || leases[1].releases.Load() != 1 {
		t.Fatalf("shutdown release counts = %d/%d", leases[0].releases.Load(), leases[1].releases.Load())
	}
	c.stop()
	if leases[0].releases.Load() != 1 || leases[1].releases.Load() != 1 {
		t.Fatalf("repeated shutdown release counts = %d/%d", leases[0].releases.Load(), leases[1].releases.Load())
	}
}

func TestDynamicSessionLoadAcrossServerResumesCurrentGeneration(t *testing.T) {
	cwd := t.TempDir()
	store := t.TempDir()
	firstSource := newDynamicLaunchSource(t, 1, "")
	first := initializeDynamicACP(t, firstSource, store)
	sid := newDynamicSession(t, first, 2, cwd)
	requirePromptSuccess(t, dynamicPrompt(t, first, 3, sid, "persist"))
	first.stop()

	secondSource := newDynamicLaunchSource(t, 2, "")
	second := initializeDynamicACP(t, secondSource, store)
	loaded := second.call(t, 4, "session/load", map[string]any{
		"sessionId":  sid,
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if loaded["error"] != nil {
		t.Fatalf("session/load: %v", loaded)
	}
	leases, starts := secondSource.snapshot()
	if len(leases) != 1 || len(starts) != 1 || starts[0].generation != 2 {
		t.Fatalf("loaded launch: leases=%d starts=%v", len(leases), starts)
	}
	if got := flagValue(starts[0].args, "--resume"); got != "claude-sess-1" {
		t.Fatalf("loaded resume = %q, args=%v", got, starts[0].args)
	}
	second.stop()
	if got := leases[0].releases.Load(); got != 1 {
		t.Fatalf("loaded lease release count = %d", got)
	}
}

func TestDynamicSessionLoadRejectsBusySession(t *testing.T) {
	source := newDynamicLaunchSource(t, 1, "hang")
	c := initializeDynamicACP(t, source, "")
	sid := newDynamicSession(t, c, 2, t.TempDir())
	sendDynamicPrompt(t, c, 3, sid, "busy")
	waitForDynamicCondition(t, func() bool {
		leases, _ := source.snapshot()
		return len(leases) >= 2
	})

	loaded := c.call(t, 4, "session/load", map[string]any{
		"sessionId":  sid,
		"mcpServers": []any{},
	})
	if rpcErrorCode(loaded) != errInternal || rpcErrorMessage(loaded) != "session is busy" {
		t.Fatalf("busy session/load = %v", loaded)
	}
	leases, _ := source.snapshot()
	if len(leases) != 2 {
		t.Fatalf("busy load acquired another lease: %d", len(leases))
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	cancelled := readDynamicPrompt(t, c, 3)
	result, _ := cancelled["result"].(map[string]any)
	if result["stopReason"] != stopCancelled {
		t.Fatalf("cancelled prompt = %v", cancelled)
	}
}
