package webui

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

// EffortLevels are the reasoning effort levels Grok Build accepts.
var EffortLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// LaunchSpec is everything one conversation turn needs.
type LaunchSpec struct {
	Provider string
	// Subscription is set for an OAuth provider; its result says so.
	Subscription bool
	// Model is a slot alias (opus, sonnet, …) or a full model id.
	Model     string
	Effort    string
	Workspace string
	// Resume is the Grok ACP session to continue; empty starts a new one.
	Resume string
	Prompt string
	// PermissionMode is the turn's permission mode. Every mode except
	// bypassPermissions asks the page before tools that require approval.
	PermissionMode string
	// Attachments are files sent with the prompt.
	Attachments []LaunchAttachment
	// attachmentViews is what the stored user message shows of them.
	attachmentViews []Attachment
	// runtimeUsage is populated by DefaultLauncher when a managed ccl runtime
	// serves the turn, providing fallback token counts if ACP omits Usage.
	runtimeUsage *func() Usage
	// routeDefaults is populated by DefaultLauncher for configured providers so
	// resumed turns reset model/effort back to provider/model defaults when the
	// user selects Auto/Default.
	routeDefaults    *acpRouteDefaults
	onRuntimeCatalog func(name string, models []string, labels map[string]string, details map[string]protocol.ModelInfo)
}

type acpRouteDefaults struct {
	model  string
	effort string
}

// LaunchAttachment is one attached file as the process sees it.
type LaunchAttachment struct {
	Path      string
	Name      string
	MediaType string
	// Image sends the file as an ACP image block; otherwise Grok gets its path.
	Image bool
}

// LaunchFunc prepares the Grok Build ACP process for one turn. Stdio is wired
// by the caller through cmd. It must not touch the caller's stdio. The returned
// cleanup runs when the process has exited.
type LaunchFunc func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error)

// Runner executes turns for a session and serializes them.
type Runner struct {
	// Prepare launches Grok Build. It is a field so tests can inject a fake.
	Prepare LaunchFunc
	// MaxConcurrency bounds how many runs execute at once across sessions.
	MaxConcurrency int
	// OnRuntimeCatalog records live runtime model metadata discovered during
	// turn preparation so /api/config and /api/providers/{name}/models stay fresh.
	OnRuntimeCatalog func(name string, models []string, labels map[string]string, details map[string]protocol.ModelInfo)

	sem chan struct{}

	mu sync.Mutex
	// latest is the most recent init event: what Grok Build offers.
	latest InitPayload
}

// Capabilities is the most recent init event any turn reported.
func (r *Runner) Capabilities() InitPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.latest
	if len(out.SlashCommands) > 0 {
		out.SlashCommands = append([]string(nil), out.SlashCommands...)
	}
	if len(out.MCPServers) > 0 {
		out.MCPServers = append([]MCPServer(nil), out.MCPServers...)
	}
	return out
}

func (r *Runner) remember(init InitPayload) {
	r.mu.Lock()
	if len(init.SlashCommands) == 0 && len(r.latest.SlashCommands) > 0 {
		init.SlashCommands = append([]string(nil), r.latest.SlashCommands...)
	}
	if len(init.MCPServers) == 0 && len(r.latest.MCPServers) > 0 {
		init.MCPServers = append([]MCPServer(nil), r.latest.MCPServers...)
	}
	r.latest = init
	r.mu.Unlock()
}

// NewRunner returns a runner using the real ccl Grok Build launcher.
func NewRunner(maxConcurrency int) *Runner {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	return &Runner{Prepare: DefaultLauncher, MaxConcurrency: maxConcurrency, sem: make(chan struct{}, maxConcurrency)}
}

func effectivePermissionMode(mode string) string {
	trimmed := strings.TrimSpace(mode)
	if trimmed == "" {
		return "bypassPermissions"
	}
	return trimmed
}

func applyWorkspaceSandboxEnv(env []string, workspace string) []string {
	if !worktreeUsesCustomSandbox(workspace) {
		return env
	}
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "GROK_SANDBOX") {
			out = append(out, "GROK_SANDBOX="+worktreeSandboxProfile)
			replaced = true
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, "GROK_SANDBOX="+worktreeSandboxProfile)
	}
	return out
}

// DefaultLauncher prepares Grok Build over ACP stdio with the conversation's
// selected provider, model, effort and permission mode. It never writes
// ~/.ccl/config.yaml.
func DefaultLauncher(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load ccl config: %w", err)
	}
	grokPath, err := groklaunch.LookPath()
	if err != nil {
		return nil, nil, fmt.Errorf("grok CLI not found in PATH: %w", err)
	}
	mode := effectivePermissionMode(spec.PermissionMode)
	name := strings.TrimSpace(spec.Provider)
	if name == "" || provider.IsDefaultProvider(name) {
		if strings.TrimSpace(cfg.LegacyClaudeDefault) != "" {
			return nil, nil, errors.New("the default Claude account has been retired; run ccl use default to select Grok Build's own login, or choose another provider")
		}
		args := buildArgs(spec)
		cmd := process.Command(grokPath, args...)
		cmd.Env = applyWorkspaceSandboxEnv(groklaunch.NativeBrowserEnv(os.Environ(), mode), spec.Workspace)
		return cmd, func() {}, nil
	}
	p, ok := cfg.Providers[name]
	if !ok || name == "" {
		return nil, nil, fmt.Errorf("provider %q not found; use ccl ls or ccl set", name)
	}
	p.Name = name
	if err := provider.ValidateRuntime(p); err != nil {
		return nil, nil, err
	}
	launch, err := groklaunch.PrepareBrowserWithMode(ctx, grokPath, p, "", mode)
	if err != nil {
		return nil, nil, err
	}
	if spec.runtimeUsage != nil && launch.Session != nil && launch.Session.Runtime != nil {
		rt := launch.Session.Runtime
		*spec.runtimeUsage = func() Usage { return usageFromRuntime(rt) }
	}
	if spec.onRuntimeCatalog != nil && launch.Session != nil && launch.Session.Runtime != nil {
		rt := launch.Session.Runtime
		spec.onRuntimeCatalog(name, rt.Models(), rt.ModelDisplayNames(), rt.ModelDetails())
	}
	if spec.routeDefaults != nil {
		defaultModel := stripOneM(launch.DefaultModel)
		targetModel := stripOneM(spec.Model)
		if targetModel == "" {
			targetModel = defaultModel
		}
		*spec.routeDefaults = acpRouteDefaults{
			model:  defaultModel,
			effort: resolveManagedDefaultEffort(p, targetModel, launch.Session),
		}
	}
	args := buildArgs(spec)
	cmd := launch.Command(args...)
	cmd.Env = applyWorkspaceSandboxEnv(cmd.Env, spec.Workspace)
	return cmd, launch.Close, nil
}

func resolveManagedDefaultEffort(p provider.Provider, model string, session *providersession.Session) string {
	explicit := groklaunch.NormalizeReasoningEffort(p.EffortLevel)
	if explicit == "none" {
		explicit = ""
	}
	model = strings.TrimSpace(model)
	stripped := stripOneM(model)
	if stripped == "" {
		return explicit
	}

	_, _, staticDetails, mdProviderID := oauthproxy.StaticProviderModelDetails(p)
	var detail protocol.ModelInfo
	hasDetail := false
	if session != nil && session.Runtime != nil {
		runtimeDetails := session.Runtime.ModelDetails()
		if info, ok := runtimeDetails[strings.ToLower(model)]; ok {
			detail, hasDetail = info, true
		} else if info, ok := runtimeDetails[strings.ToLower(stripped)]; ok {
			detail, hasDetail = info, true
		}
		if !hasDetail {
			for id, info := range runtimeDetails {
				if strings.EqualFold(id, model) || strings.EqualFold(id, stripped) {
					detail, hasDetail = info, true
					break
				}
			}
		}
	}
	if !hasDetail {
		if info, ok := staticDetails[strings.ToLower(model)]; ok {
			detail, hasDetail = info, true
		} else if info, ok := staticDetails[strings.ToLower(stripped)]; ok {
			detail, hasDetail = info, true
		}
	}

	displayName := lookupCaseInsensitiveLabel(p.ModelLabels, model)
	if displayName == "" {
		displayName = lookupCaseInsensitiveLabel(p.ModelLabels, stripped)
	}
	if displayName == "" && session != nil && session.Runtime != nil {
		displayName = lookupCaseInsensitiveLabel(session.Runtime.ModelDisplayNames(), model)
		if displayName == "" {
			displayName = lookupCaseInsensitiveLabel(session.Runtime.ModelDisplayNames(), stripped)
		}
	}
	if displayName == "" {
		displayName = strings.TrimSpace(detail.DisplayName)
	}

	index, _ := modelsdev.CachedIndex()
	supports, levels, defaultEffort := oauthproxy.ResolveGrokModelEffort(index, mdProviderID, model, displayName, detail, hasDetail)
	if !supports || len(levels) == 0 {
		if explicit != "" && (!hasDetail || !detail.ReasoningKnown) {
			return explicit
		}
		return ""
	}
	if explicit != "" {
		return pickManagedEffort(levels, explicit)
	}
	if defaultEffort != "" {
		return defaultEffort
	}
	return pickManagedEffort(levels, "medium")
}

func pickManagedEffort(levels []string, target string) string {
	for _, lvl := range levels {
		if lvl == target {
			return lvl
		}
	}
	rank := map[string]int{
		"minimal": 1,
		"low":     2,
		"medium":  3,
		"high":    4,
		"xhigh":   5,
		"max":     6,
		"ultra":   7,
	}
	wantRank, ok := rank[target]
	if !ok {
		return levels[0]
	}
	best := ""
	bestRank := -1
	for _, lvl := range levels {
		r := rank[lvl]
		if r <= wantRank && r > bestRank {
			best = lvl
			bestRank = r
		}
	}
	if best != "" {
		return best
	}
	return levels[0]
}

func usageFromRuntime(rt *oauthproxy.Runtime) Usage {
	if rt == nil {
		return Usage{}
	}
	totals, ok := rt.Usage().Snapshot()
	if !ok {
		return Usage{}
	}
	var out Usage
	for _, item := range totals {
		out.InputTokens += item.InputTokens + item.CacheReadTokens + item.CacheWriteTokens
		out.OutputTokens += item.OutputTokens
	}
	return out
}

// buildArgs is the Grok Build ACP command line for one turn.
func buildArgs(spec LaunchSpec) []string {
	mode := strings.TrimSpace(spec.PermissionMode)
	if mode == "" {
		mode = "bypassPermissions"
	}
	args := []string{"agent", "--no-leader"}
	if model := stripOneM(spec.Model); model != "" {
		args = append(args, "-m", model)
	}
	if effort := groklaunch.NormalizeReasoningEffort(spec.Effort); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if mode == "bypassPermissions" {
		args = append(args, "--always-approve")
	}
	args = append(args, "stdio")
	return args
}

// promptBlocks encodes the turn's prompt and attachments as ACP ContentBlocks:
// images as base64 image blocks first, then the text block listing any non-image
// attached file paths.
func promptBlocks(spec LaunchSpec) ([]acp.ContentBlock, error) {
	blocks := make([]acp.ContentBlock, 0, len(spec.Attachments)+1)
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
		blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(data), attachment.MediaType))
	}
	text := spec.Prompt
	if len(files) > 0 {
		text = strings.TrimSpace(text + "\n\nAttached files:\n" + strings.Join(files, "\n"))
	}
	if text != "" || len(blocks) == 0 {
		blocks = append(blocks, acp.TextBlock(text))
	}
	return blocks, nil
}

type gatedReader struct {
	r     io.Reader
	ready chan struct{}
	once  sync.Once
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.once.Do(func() { <-g.ready })
	return g.r.Read(p)
}

// ErrBusy reports a turn already running for the session.
var ErrBusy = errors.New("a turn is already running")

// turn is one in-flight run.
type turn struct {
	cmd       *exec.Cmd
	conn      *acp.ClientSideConnection
	sessionID acp.SessionId
	cancel    context.CancelFunc
	done      chan struct{}
	// ended closes once the turn's result is stored: the process is only
	// exiting, so the next request waits for done instead of being busy.
	ended   chan struct{}
	endOnce sync.Once
	// subscription is copied to the turn's result (LaunchSpec.Subscription).
	subscription bool
	// abort wakes a turn waiting for a global slot or a pending permission.
	abort     chan struct{}
	abortOnce sync.Once
	// cancelRequested records a user stop so the exit is not reported as a crash.
	cancelRequested bool
	mu              sync.Mutex

	stdin   io.Closer
	closed  bool
	pending map[string]pendingPermission
}

func (e *turn) triggerAbort() {
	e.abortOnce.Do(func() { close(e.abort) })
}

func (e *turn) closeStdin() {
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
	// Cancel asks the ACP session and process to stop.
	Cancel func()
}

// Timeline is the per-session turn gate.
type Timeline struct {
	mu      sync.Mutex
	running map[string]*turn
}

// NewTimeline returns an empty turn gate.
func NewTimeline() *Timeline { return &Timeline{running: make(map[string]*turn)} }

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

// Running reports whether the session has a turn in flight.
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

// Steer reports whether a running turn can accept an inline message. ACP v1
// processes one prompt per turn, so a live turn returns ErrBusy and an ended
// turn returns errTurnEnded.
func (t *Timeline) Steer(id string, _ LaunchSpec) error {
	t.mu.Lock()
	entry := t.running[id]
	t.mu.Unlock()
	if entry == nil {
		return errTurnEnded
	}
	select {
	case <-entry.ended:
		return errTurnEnded
	default:
		return ErrBusy
	}
}

// Start runs one turn: it stores the user message, launches Grok Build over
// ACP, and stores every translated event. It returns ErrBusy when a turn
// already runs.
func (t *Timeline) Start(r *Runner, session *Session, spec LaunchSpec) (*TurnHandle, error) {
	id := session.ID()

	t.mu.Lock()
	if _, busy := t.running[id]; busy {
		t.mu.Unlock()
		return nil, ErrBusy
	}
	entry := &turn{
		done:         make(chan struct{}),
		ended:        make(chan struct{}),
		abort:        make(chan struct{}),
		subscription: spec.Subscription,
		pending:      make(map[string]pendingPermission),
	}
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
		_ = t.launch(r, session, entry, spec, relinquish)
	}()
	return handle, nil
}

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
		_, _ = session.Append(EventError, ErrorPayload{Message: redactSecrets(err.Error())})
		_, _ = session.Append(EventResult, ResultPayload{IsError: true})
		_ = session.Touch()
		relinquish()
		return err
	}

	if stopped {
		cancel()
		<-r.sem
		_, _ = session.Append(EventResult, ResultPayload{Cancelled: true, Message: "cancelled before it started"})
		_ = session.Touch()
		relinquish()
		return nil
	}

	if strings.TrimSpace(spec.Workspace) == "" {
		if ws := strings.TrimSpace(session.Meta().Workspace); ws != "" {
			spec.Workspace = ws
		} else if wd, wdErr := os.Getwd(); wdErr == nil {
			spec.Workspace = wd
		}
	}

	blocks, err := promptBlocks(spec)
	if err != nil {
		return fail(nil, err)
	}

	var fallbackUsage func() Usage
	spec.runtimeUsage = &fallbackUsage
	var routeDefaults acpRouteDefaults
	spec.routeDefaults = &routeDefaults
	spec.onRuntimeCatalog = r.OnRuntimeCatalog

	cmd, cleanup, err := r.Prepare(ctx, spec)
	if err != nil {
		return fail(nil, err)
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Grok stdout: %w", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Grok stderr: %w", err))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(cleanup, fmt.Errorf("attach Grok stdin: %w", err))
	}
	cmd.Dir = spec.Workspace
	process.ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fail(cleanup, fmt.Errorf("start Grok Build: %w", err))
	}

	client := newACPTurnClient(r, session, entry, spec.Workspace, spec.Model, effectivePermissionMode(spec.PermissionMode))
	gatedStdout := &gatedReader{r: stdout, ready: make(chan struct{})}
	conn := acp.NewClientSideConnection(client, stdin, gatedStdout)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	close(gatedStdout.ready)

	entry.mu.Lock()
	entry.cmd = cmd
	entry.conn = conn
	entry.stdin = stdin
	stopped = entry.cancelRequested
	entry.mu.Unlock()
	if stopped {
		t.cancel(entry)
	}

	go t.pump(r, session, entry, spec, blocks, client, conn, stderr, fallbackUsage, cleanup, cancel, relinquish)
	return nil
}

func (t *Timeline) cancel(entry *turn) {
	entry.mu.Lock()
	alreadyRequested := entry.cancelRequested
	entry.cancelRequested = true
	conn := entry.conn
	sessionID := entry.sessionID
	cmd := entry.cmd
	cancel := entry.cancel
	entry.mu.Unlock()
	if alreadyRequested {
		return
	}
	entry.triggerAbort()
	if conn != nil && sessionID != "" {
		go func() {
			cancelCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			_ = conn.Cancel(cancelCtx, acp.CancelNotification{SessionId: sessionID})
			select {
			case <-entry.done:
				return
			case <-time.After(2 * time.Second):
			}
			if cancel != nil {
				cancel()
			}
			entry.closeStdin()
			if cmd != nil {
				interrupt(cmd, entry.done)
			}
		}()
		return
	}
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

func (t *Timeline) pump(
	r *Runner,
	session *Session,
	entry *turn,
	spec LaunchSpec,
	blocks []acp.ContentBlock,
	client *acpTurnClient,
	conn *acp.ClientSideConnection,
	stderr io.Reader,
	fallbackUsage func() Usage,
	cleanup func(),
	cancel context.CancelFunc,
	relinquish func(),
) {
	defer relinquish()
	defer cancel()
	defer cleanup()
	defer func() { <-r.sem }()

	turnStart := time.Now()
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

	var promptResp acp.PromptResponse
	runErr := func() error {
		setupCtx, stopSetup := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopSetup()
		go func() {
			select {
			case <-entry.abort:
				stopSetup()
			case <-setupCtx.Done():
			}
		}()

		info, err := conn.Initialize(setupCtx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
		if err != nil {
			return fmt.Errorf("initialize Grok Build ACP: %w", err)
		}
		if info.ProtocolVersion != acp.ProtocolVersionNumber {
			return fmt.Errorf("unsupported Grok Build ACP protocol version %d", info.ProtocolVersion)
		}
		client.seedSlashCommands(slashCommandsFromInitMeta(info.Meta))
		if hasNonEmptyEnv(entry.cmd.Env, "XAI_API_KEY") && supportsAPIKeyAuth(info.AuthMethods) {
			if _, err := conn.Authenticate(setupCtx, acp.AuthenticateRequest{
				MethodId: "xai.api_key",
				Meta:     map[string]any{"headless": true},
			}); err != nil {
				return fmt.Errorf("authenticate Grok Build runtime: %w", err)
			}
		}

		mode := effectivePermissionMode(spec.PermissionMode)
		sessionMeta := sessionMetaForMode(mode)

		var sessionID acp.SessionId
		var configOptions []acp.SessionConfigOption
		resumed := strings.TrimSpace(spec.Resume) != ""
		if resumed {
			if !info.AgentCapabilities.LoadSession {
				return errors.New("installed Grok Build does not support ACP session loading")
			}
			sessionID = acp.SessionId(strings.TrimSpace(spec.Resume))
			client.setSession(sessionID, true)
			loaded, err := conn.LoadSession(setupCtx, acp.LoadSessionRequest{
				SessionId:  sessionID,
				Cwd:        spec.Workspace,
				McpServers: []acp.McpServer{},
				Meta:       sessionMeta,
			})
			client.setLoading(false)
			if err != nil {
				return fmt.Errorf("load Grok Build session: %w", err)
			}
			configOptions = loaded.ConfigOptions
		} else {
			created, err := conn.NewSession(setupCtx, acp.NewSessionRequest{
				Cwd:        spec.Workspace,
				McpServers: []acp.McpServer{},
				Meta:       sessionMeta,
			})
			if err != nil {
				return fmt.Errorf("open Grok Build session: %w", err)
			}
			if created.SessionId == "" {
				return errors.New("Grok Build returned an empty session ID")
			}
			sessionID = created.SessionId
			configOptions = created.ConfigOptions
			client.setSession(sessionID, false)
		}

		targetModel := stripOneM(spec.Model)
		targetEffort := groklaunch.NormalizeReasoningEffort(spec.Effort)
		if resumed && spec.routeDefaults != nil {
			if targetModel == "" {
				targetModel = spec.routeDefaults.model
			}
			if targetEffort == "" {
				targetEffort = spec.routeDefaults.effort
			}
		}
		applyACPSessionRoute(setupCtx, conn, sessionID, configOptions, targetModel, targetEffort, resumed)
		applyACPSessionMode(setupCtx, conn, sessionID, mode)
		mcpList := fetchSessionMCPServers(setupCtx, conn, sessionID, spec.Workspace, info.Meta)
		client.setMCPServers(mcpList)

		entry.mu.Lock()
		entry.sessionID = sessionID
		stoppedNow := entry.cancelRequested
		entry.mu.Unlock()

		initModel := stripOneM(spec.Model)
		if initModel == "" {
			initModel = targetModel
		}
		initPayload := InitPayload{
			SessionID:     string(sessionID),
			Model:         initModel,
			Cwd:           spec.Workspace,
			SlashCommands: client.slashCommands(),
			MCPServers:    mcpList,
		}
		r.remember(initPayload)
		_, _ = session.UpdateMeta(func(meta *Meta) {
			if meta.AgentSessionID == "" {
				meta.AgentSessionID = string(sessionID)
			}
		})
		_, _ = session.Append(EventInit, initPayload)

		if stoppedNow {
			return nil
		}

		promptCtx, stopPrompt := context.WithCancel(context.Background())
		defer stopPrompt()
		go func() {
			select {
			case <-entry.abort:
				// Allow conn.Cancel a brief window to return StopReasonCancelled cleanly.
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				select {
				case <-promptCtx.Done():
				case <-timer.C:
					stopPrompt()
				}
			case <-promptCtx.Done():
			}
		}()

		resp, err := conn.Prompt(promptCtx, acp.PromptRequest{
			SessionId: sessionID,
			Prompt:    blocks,
		})
		if err != nil {
			return err
		}
		promptResp = resp
		return nil
	}()

	durationMs := time.Since(turnStart).Milliseconds()
	usage := acpTurnUsage(promptResp.Usage, fallbackUsage, client.sessionCostUSD(), durationMs)

	entry.closeStdin()
	waitDone := make(chan error, 1)
	go func() { waitDone <- entry.cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-time.After(2 * time.Second):
		process.KillTree(entry.cmd)
		waitErr = <-waitDone
	}
	<-stderrDone

	stopped := cancelled() || promptResp.StopReason == acp.StopReasonCancelled
	stderrMsg := strings.TrimSpace(strings.Join(stderrText, "\n"))

	result := ResultPayload{
		Usage:        usage,
		Cancelled:    stopped,
		Subscription: entry.subscription,
	}
	switch {
	case stopped:
		// User-cancelled turn is not an error.
	case runErr != nil:
		result.IsError = true
		errText := strings.TrimSpace(runErr.Error())
		if stderrMsg != "" && !strings.Contains(stderrMsg, errText) {
			errText = stderrMsg + "\n" + errText
		} else if stderrMsg != "" {
			errText = stderrMsg
		}
		errText = truncateRunes(redactSecrets(errText), 4000)
		_, _ = session.Append(EventError, ErrorPayload{Message: errText})
		result.Message = truncateRunes(errText, SummaryLimit)
	case promptResp.StopReason == acp.StopReasonRefusal:
		result.IsError = true
		result.Message = "refusal"
	case waitErr != nil && stderrMsg != "":
		// Process exited non-zero after completing or dropping stdio.
		if promptResp.StopReason == "" {
			result.IsError = true
			msg := truncateRunes(redactSecrets(stderrMsg), 4000)
			_, _ = session.Append(EventError, ErrorPayload{Message: msg})
			result.Message = truncateRunes(msg, SummaryLimit)
		}
	default:
		if promptResp.StopReason != "" && promptResp.StopReason != acp.StopReasonEndTurn {
			result.Message = string(promptResp.StopReason)
		}
	}

	_, _ = session.UpdateMeta(func(meta *Meta) {
		turn := meta.recordTurn(result.Usage)
		result.TurnUsage = &turn
	})
	_, _ = session.Append(EventResult, result)
	entry.endOnce.Do(func() { close(entry.ended) })
	_ = session.Touch()
}

func acpTurnUsage(reported *acp.Usage, fallback func() Usage, sessionCostUSD float64, durationMs int64) Usage {
	out := Usage{DurationMs: durationMs, CostUSD: sessionCostUSD}
	if reported != nil && (reported.InputTokens > 0 || reported.OutputTokens > 0) {
		out.InputTokens = int64(reported.InputTokens)
		if reported.CachedReadTokens != nil {
			out.InputTokens += int64(*reported.CachedReadTokens)
		}
		if reported.CachedWriteTokens != nil {
			out.InputTokens += int64(*reported.CachedWriteTokens)
		}
		out.OutputTokens = int64(reported.OutputTokens)
		return out
	}
	if fallback != nil {
		fb := fallback()
		out.InputTokens = fb.InputTokens
		out.OutputTokens = fb.OutputTokens
		if out.CostUSD == 0 {
			out.CostUSD = fb.CostUSD
		}
	}
	return out
}

func hasNonEmptyEnv(env []string, target string) bool {
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, target) && strings.TrimSpace(val) != "" {
			return true
		}
	}
	return false
}

func supportsAPIKeyAuth(methods []acp.AuthMethod) bool {
	for _, method := range methods {
		if (method.Agent != nil && method.Agent.Id == "xai.api_key") ||
			(method.EnvVar != nil && method.EnvVar.Id == "xai.api_key") {
			return true
		}
	}
	return false
}

func sessionMetaForMode(mode string) map[string]any {
	switch effectivePermissionMode(mode) {
	case "bypassPermissions":
		return map[string]any{"yoloMode": true}
	case "acceptEdits":
		return map[string]any{"autoMode": true, "yoloMode": false}
	case "plan", "default":
		return map[string]any{"yoloMode": false}
	default:
		return map[string]any{"yoloMode": false}
	}
}

func acpModeIDForPermissionMode(mode string) acp.SessionModeId {
	switch effectivePermissionMode(mode) {
	case "bypassPermissions":
		return "bypassPermissions"
	case "acceptEdits":
		return "auto"
	case "plan":
		return "plan"
	default:
		return "default"
	}
}

func applyACPSessionMode(ctx context.Context, conn *acp.ClientSideConnection, sessionID acp.SessionId, mode string) {
	if conn == nil || sessionID == "" {
		return
	}
	modeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = conn.SetSessionMode(modeCtx, acp.SetSessionModeRequest{
		SessionId: sessionID,
		ModeId:    acpModeIDForPermissionMode(mode),
	})
}

var grokEffortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// applyACPSessionRoute syncs the conversation's requested model and reasoning
// effort onto the ACP session. Grok Build's session/load restores the saved
// session's Turn-1 model and effort over CLI -m / --reasoning-effort, so
// resumed turns must apply session/set_config_option explicitly.
func applyACPSessionRoute(ctx context.Context, conn *acp.ClientSideConnection, sessionID acp.SessionId, options []acp.SessionConfigOption, model, effort string, resumed bool) {
	if conn == nil || sessionID == "" {
		return
	}
	setOption := func(configID, value string) ([]acp.SessionConfigOption, bool) {
		optCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := conn.SetSessionConfigOption(optCtx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: sessionID,
				ConfigId:  acp.SessionConfigId(configID),
				Value:     acp.SessionConfigValueId(value),
			},
		})
		if err != nil {
			return nil, false
		}
		return resp.ConfigOptions, true
	}

	if model != "" {
		if sel := findSelectConfigOption(options, "model"); sel != nil {
			target := matchExactOptionValue(selectableConfigValues(sel), model)
			if target != "" && string(sel.CurrentValue) != target {
				if updated, ok := setOption("model", target); ok && len(updated) > 0 {
					options = updated
				}
			}
		} else if resumed && len(options) == 0 {
			if updated, ok := setOption("model", model); ok && len(updated) > 0 {
				options = updated
			}
		}
	}

	if effort != "" {
		if sel := findSelectConfigOption(options, "reasoning_effort"); sel != nil {
			target := matchAllowedEffort(selectableConfigValues(sel), effort)
			if target != "" && !strings.EqualFold(string(sel.CurrentValue), target) {
				_, _ = setOption("reasoning_effort", target)
			}
		} else if resumed && len(options) == 0 {
			_, _ = setOption("reasoning_effort", effort)
		}
	}
}

func findSelectConfigOption(options []acp.SessionConfigOption, id string) *acp.SessionConfigOptionSelect {
	for i := range options {
		sel := options[i].Select
		if sel != nil && string(sel.Id) == id {
			return sel
		}
	}
	return nil
}

func selectableConfigValues(sel *acp.SessionConfigOptionSelect) []string {
	if sel == nil {
		return nil
	}
	var out []string
	if sel.Options.Ungrouped != nil {
		for _, opt := range *sel.Options.Ungrouped {
			if v := strings.TrimSpace(string(opt.Value)); v != "" {
				out = append(out, v)
			}
		}
	}
	if sel.Options.Grouped != nil {
		for _, grp := range *sel.Options.Grouped {
			for _, opt := range grp.Options {
				if v := strings.TrimSpace(string(opt.Value)); v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

func matchExactOptionValue(allowed []string, desired string) string {
	if len(allowed) == 0 {
		return desired
	}
	for _, candidate := range allowed {
		if candidate == desired {
			return candidate
		}
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, desired) {
			return candidate
		}
	}
	return ""
}

func matchAllowedEffort(allowed []string, desired string) string {
	if len(allowed) == 0 {
		return ""
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, desired) {
			return candidate
		}
	}
	desiredIdx := -1
	for i, tier := range grokEffortLadder {
		if strings.EqualFold(tier, desired) {
			desiredIdx = i
			break
		}
	}
	if desiredIdx < 0 {
		return ""
	}
	for i := desiredIdx - 1; i >= 0; i-- {
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, grokEffortLadder[i]) {
				return candidate
			}
		}
	}
	for i := desiredIdx + 1; i < len(grokEffortLadder); i++ {
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, grokEffortLadder[i]) {
				return candidate
			}
		}
	}
	return ""
}

func slashCommandsFromInitMeta(meta map[string]any) []string {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta["availableCommands"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil
	}
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		name := strings.TrimPrefix(strings.TrimSpace(item.Name), "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func fetchSessionMCPServers(ctx context.Context, conn *acp.ClientSideConnection, sessionID acp.SessionId, workspace string, initMeta map[string]any) []MCPServer {
	if conn != nil && sessionID != "" {
		extCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		raw, err := conn.CallExtension(extCtx, "_x.ai/mcp/list", map[string]any{"sessionId": string(sessionID)})
		cancel()
		if err == nil && len(raw) > 0 {
			if parsed := parseACPMCPList(raw); len(parsed) > 0 {
				return parsed
			}
		}
	}
	if len(initMeta) > 0 {
		if raw, ok := initMeta["mcpServers"]; ok && raw != nil {
			if encoded, err := json.Marshal(raw); err == nil {
				if parsed := parseACPMCPList(encoded); len(parsed) > 0 {
					return parsed
				}
			}
		}
	}
	return readWorkspaceMCPConfigSummary(workspace)
}

func parseACPMCPList(raw []byte) []MCPServer {
	var envelope struct {
		Servers []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Session *struct {
				Enabled bool   `json:"enabled"`
				Status  string `json:"status"`
			} `json:"session"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		var flat []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil
		}
		out := make([]MCPServer, 0, len(flat))
		seen := make(map[string]bool, len(flat))
		for _, srv := range flat {
			name := strings.TrimSpace(srv.Name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			status := strings.TrimSpace(srv.Status)
			if status == "" {
				status = "configured"
			}
			out = append(out, MCPServer{Name: name, Status: status})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	out := make([]MCPServer, 0, len(envelope.Servers))
	seen := make(map[string]bool, len(envelope.Servers))
	for _, srv := range envelope.Servers {
		name := strings.TrimSpace(srv.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		status := strings.TrimSpace(srv.Status)
		if srv.Session != nil {
			if !srv.Session.Enabled {
				status = "disabled"
			} else if strings.TrimSpace(srv.Session.Status) != "" {
				status = strings.TrimSpace(srv.Session.Status)
			}
		}
		if status == "" {
			status = "configured"
		}
		out = append(out, MCPServer{Name: name, Status: status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readWorkspaceMCPConfigSummary(workspace string) []MCPServer {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.MCPServers) == 0 {
		return nil
	}
	out := make([]MCPServer, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			out = append(out, MCPServer{Name: trimmed, Status: "configured"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type acpTurnClient struct {
	runner    *Runner
	session   *Session
	entry     *turn
	workspace string
	model     string

	mu         sync.Mutex
	sessionID  acp.SessionId
	loading    bool
	mode       string
	mcp        []MCPServer
	translator *acpTranslator
}

func newACPTurnClient(r *Runner, session *Session, entry *turn, workspace, model, mode string) *acpTurnClient {
	return &acpTurnClient{
		runner:     r,
		session:    session,
		entry:      entry,
		workspace:  workspace,
		model:      model,
		mode:       effectivePermissionMode(mode),
		translator: newACPTranslator(),
	}
}

func (c *acpTurnClient) setSession(id acp.SessionId, loading bool) {
	c.mu.Lock()
	c.sessionID = id
	c.loading = loading
	c.mu.Unlock()
}

func (c *acpTurnClient) setLoading(loading bool) {
	c.mu.Lock()
	c.loading = loading
	c.mu.Unlock()
}

func (c *acpTurnClient) setPermissionMode(mode string) {
	c.mu.Lock()
	c.mode = effectivePermissionMode(mode)
	c.mu.Unlock()
}

func (c *acpTurnClient) seedSlashCommands(commands []string) {
	if len(commands) == 0 {
		return
	}
	c.mu.Lock()
	if len(c.translator.slashCommands) == 0 {
		c.translator.slashCommands = append([]string(nil), commands...)
	}
	c.mu.Unlock()
}

func (c *acpTurnClient) setMCPServers(servers []MCPServer) {
	c.mu.Lock()
	c.mcp = append([]MCPServer(nil), servers...)
	c.mu.Unlock()
}

func (c *acpTurnClient) mcpServers() []MCPServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MCPServer(nil), c.mcp...)
}

func (c *acpTurnClient) slashCommands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.translator.SlashCommands()
}

func (c *acpTurnClient) sessionCostUSD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.translator.SessionCostUSD()
}

func (c *acpTurnClient) SessionUpdate(_ context.Context, note acp.SessionNotification) error {
	c.mu.Lock()
	if c.sessionID != "" && note.SessionId != c.sessionID {
		c.mu.Unlock()
		return nil
	}
	if note.Update.AvailableCommandsUpdate != nil {
		commands, changed := c.translator.HandleCommands(note.Update.AvailableCommandsUpdate)
		loading := c.loading
		sid := string(c.sessionID)
		mcp := append([]MCPServer(nil), c.mcp...)
		c.mu.Unlock()
		if changed {
			initPayload := InitPayload{
				SessionID:     sid,
				Model:         stripOneM(c.model),
				Cwd:           c.workspace,
				SlashCommands: commands,
				MCPServers:    mcp,
			}
			c.runner.remember(initPayload)
			if !loading && sid != "" {
				_, _ = c.session.Append(EventInit, initPayload)
			}
		}
		return nil
	}
	if c.loading {
		// Stored events.jsonl already holds prior turns; skip LoadSession replay chunks.
		c.mu.Unlock()
		return nil
	}
	type pair struct {
		kind    string
		payload any
	}
	var events []pair
	c.translator.Handle(note.Update, func(kind string, payload any) {
		events = append(events, pair{kind: kind, payload: payload})
	})
	c.mu.Unlock()

	for _, ev := range events {
		_, _ = c.session.Append(ev.kind, ev.payload)
	}
	return nil
}

var _ acp.ExtensionMethodHandler = (*acpTurnClient)(nil)

func (c *acpTurnClient) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != "_x.ai/session/update" {
		return nil, acp.NewMethodNotFound(method)
	}
	var env struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &env); err != nil || len(env.Update) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	if c.loading || (c.sessionID != "" && env.SessionID != "" && acp.SessionId(env.SessionID) != c.sessionID) {
		c.mu.Unlock()
		return nil, nil
	}
	type pair struct {
		kind    string
		payload any
	}
	var events []pair
	c.translator.HandleExtensionUpdate(env.Update, func(kind string, payload any) {
		events = append(events, pair{kind: kind, payload: payload})
	})
	c.mu.Unlock()

	for _, ev := range events {
		_, _ = c.session.Append(ev.kind, ev.payload)
	}
	return nil, nil
}

func (c *acpTurnClient) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	cancelled := acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Cancelled: &acp.RequestPermissionOutcomeCancelled{Outcome: "cancelled"},
	}}

	c.mu.Lock()
	sid := c.sessionID
	loading := c.loading
	mode := c.mode
	c.mu.Unlock()
	if loading || (sid != "" && req.SessionId != sid) {
		return cancelled, nil
	}

	requestID := uuid.NewString()
	payload, pending := buildPermissionRequest(requestID, req)

	if mode == "acceptEdits" &&
		req.ToolCall.Kind != nil && *req.ToolCall.Kind == acp.ToolKindEdit &&
		payload.ToolName != "ExitPlanMode" && payload.ToolName != "AskUserQuestion" &&
		pending.allowOnce != "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{
				Outcome:  "selected",
				OptionId: pending.allowOnce,
			},
		}}, nil
	}

	c.entry.mu.Lock()
	if c.entry.closed || c.entry.cancelRequested {
		c.entry.mu.Unlock()
		return cancelled, nil
	}
	if c.entry.pending == nil {
		c.entry.pending = make(map[string]pendingPermission)
	}
	c.entry.pending[requestID] = pending
	c.entry.mu.Unlock()

	_, _ = c.session.Append(EventPermission, payload)

	withdraw := func() {
		c.entry.mu.Lock()
		_, stillPending := c.entry.pending[requestID]
		if stillPending {
			delete(c.entry.pending, requestID)
		}
		c.entry.mu.Unlock()
		if stillPending {
			_, _ = c.session.Append(EventPermissionResolved, PermissionResolvedPayload{
				RequestID: requestID,
				Behavior:  "cancelled",
			})
		}
	}

	select {
	case <-ctx.Done():
		withdraw()
		return cancelled, nil
	case <-c.entry.abort:
		withdraw()
		return cancelled, nil
	case decision := <-pending.reply:
		if ctx.Err() != nil {
			return cancelled, nil
		}
		opt, ok := pending.optionFor(decision)
		if !ok {
			return cancelled, nil
		}
		if decision.Mode != "" {
			c.setPermissionMode(decision.Mode)
			c.entry.mu.Lock()
			conn := c.entry.conn
			c.entry.mu.Unlock()
			if conn != nil && sid != "" {
				targetMode := decision.Mode
				go applyACPSessionMode(context.Background(), conn, sid, targetMode)
			}
		}
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{
				Outcome:  "selected",
				OptionId: opt,
				Meta:     permissionOutcomeMeta(decision),
			},
		}}, nil
	}
}

// Grok Build executes tools itself inside its workspace sandbox; client-side
// filesystem and terminal RPCs are intentionally unsupported.
func (*acpTurnClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (*acpTurnClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (*acpTurnClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}

func (*acpTurnClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}

func (*acpTurnClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}

func (*acpTurnClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}

func (*acpTurnClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}

// readLines appends at most limit lines of r to out, draining the rest to EOF.
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
