package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/google/uuid"
)

//go:embed web
var webAssets embed.FS

// TokenHeader, PageTokenHeader and TokenQuery are the ways a client presents
// its token.
const (
	TokenHeader = "Authorization"
	// PageTokenHeader is what the page sends: it leaves Authorization to a
	// proxy's Basic auth in front of ccl serve. A request that carries it is
	// judged by it alone.
	PageTokenHeader = "X-CCL-Token"
	// TokenQuery is accepted only by the read-only stream and events endpoints:
	// EventSource cannot set a header.
	TokenQuery = "token"
)

// Config configures a web UI server.
type Config struct {
	// Token authenticates /api requests. It may be empty only when the server
	// listens on a loopback address and RequireToken is false.
	Token string
	// RequireToken makes an empty Token a startup error. ccl serve sets it when
	// the bind address is reachable from outside the machine.
	RequireToken bool
	// Workspace, Workspaces and WorkspaceRoots are the directories
	// conversations may run in: each named directory, and each directory
	// directly inside a root. The page may add folders unless LockWorkspaces.
	// With none, the current directory is the workspace.
	Workspace      string
	Workspaces     []string
	WorkspaceRoots []string
	// MaxConcurrency bounds simultaneous Claude Code processes. <=0 uses 3.
	MaxConcurrency int
	// DefaultProvider/DefaultModel/DefaultEffort seed new conversations.
	DefaultProvider string
	DefaultModel    string
	DefaultEffort   string
	// StoreRoot overrides the on-disk conversation directory (tests).
	StoreRoot string
	// Version is reported by /health.
	Version string
	// Launch overrides the process launcher (tests).
	Launch LaunchFunc
	// SelfBinary is the ccl executable; subscription logins run `ccl oauth`.
	SelfBinary string
	// LatestVersion names the newest ccl release, for the update notice.
	LatestVersion func() (string, error)
	// Shell turns on the web terminal (ccl serve --shell).
	Shell bool
	// LockWorkspaces keeps the page from adding folders (--lock-workspaces).
	LockWorkspaces bool
	// BasePath mounts the page and the API under a URL prefix such as /chat
	// (--base-path); empty serves them at the root.
	BasePath string
}

// Server is the HTTP surface of ccl serve.
type Server struct {
	store *Store
	// mirrorMu serializes following transcripts (mirror.go).
	mirrorMu sync.Mutex
	runner   *Runner
	timeline *Timeline
	token    string
	// workspaces are the directories conversations may run in.
	workspaces *workspaceSet
	// projects holds each project's defaults for new conversations.
	projects *projectStore
	version  string

	defaults metaDefaults

	// subscribers wakes open SSE streams when an event is appended.
	mu          sync.Mutex
	subscribers map[string][]chan struct{}

	selfBinary    string
	latestVersion func() (string, error)
	update        updateCache
	logins        loginRegistry
	schedules     *scheduler
	shell         bool
	terminals     terminalRegistry
	// basePath is the normalized URL prefix ("" at the root); index is the
	// page with its base filled in for it.
	basePath  string
	index     []byte
	indexETag string
	// closing is set by Close; no turn starts after it.
	closing atomic.Bool
}

type metaDefaults struct {
	Provider       string
	Model          string
	Effort         string
	PermissionMode string
}

// ProviderInfo is one selectable provider, as the browser sees it.
type ProviderInfo struct {
	Name        string            `json:"name"`
	IsDefault   bool              `json:"isDefault"`
	Slots       map[string]string `json:"slots"`
	EffortLevel string            `json:"effortLevel,omitempty"`
	Active      bool              `json:"active,omitempty"`
}

// SessionView is a conversation as the browser sees it. Running is known only
// to this process, so it is never stored in meta.json.
type SessionView struct {
	Meta
	Running bool `json:"running"`
}

func (s *Server) view(meta Meta) SessionView {
	return SessionView{Meta: meta, Running: s.timeline.Running(meta.ID)}
}

// New creates a server. An empty token is an error: ccl serve never listens
// without authentication.
func New(cfg Config) (*Server, error) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" && cfg.RequireToken {
		return nil, errors.New("a token is required when listening beyond 127.0.0.1: pass --token or set CCL_SERVE_TOKEN")
	}
	basePath, err := NormalizeBasePath(strings.TrimSpace(cfg.BasePath))
	if err != nil {
		return nil, err
	}
	index, err := pageIndex(basePath)
	if err != nil {
		return nil, err
	}
	var paths []string
	if workspace := strings.TrimSpace(cfg.Workspace); workspace != "" {
		paths = append(paths, workspace)
	}
	for _, workspace := range cfg.Workspaces {
		if workspace = strings.TrimSpace(workspace); workspace != "" {
			paths = append(paths, workspace)
		}
	}
	if len(paths) == 0 && len(cfg.WorkspaceRoots) == 0 {
		paths = []string{"."}
	}
	workspaces, err := newWorkspaceSet(paths, cfg.WorkspaceRoots)
	if err != nil {
		return nil, err
	}

	root := cfg.StoreRoot
	if root == "" {
		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	store, err := NewStore(root)
	if err != nil {
		return nil, err
	}
	workspaces.loadAdded(filepath.Join(root, "workspaces.json"), cfg.LockWorkspaces)
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 3
	}
	runner := NewRunner(concurrency)
	if cfg.Launch != nil {
		runner.Prepare = cfg.Launch
	}
	server := &Server{
		workspaces:    workspaces,
		projects:      loadProjects(filepath.Join(root, "projects.json")),
		store:         store,
		runner:        runner,
		timeline:      NewTimeline(),
		token:         token,
		version:       cfg.Version,
		subscribers:   make(map[string][]chan struct{}),
		selfBinary:    cfg.SelfBinary,
		latestVersion: cfg.LatestVersion,
		logins:        loginRegistry{logins: map[string]*oauthLogin{}},
		schedules:     newScheduler(schedulePath(root)),
		shell:         cfg.Shell,
		terminals:     terminalRegistry{terminals: map[string]*terminal{}},
		basePath:      basePath,
		index:         index,
		indexETag:     contentETag(index),
	}
	defProvider := strings.TrimSpace(cfg.DefaultProvider)
	// Turns ask before tools as the terminal would, unless ccl's bypass mode
	// (ccl bypass) is on.
	permissionMode := "default"
	if loaded, err := config.Load(); err == nil {
		if defProvider == "" {
			defProvider = loaded.ActiveProvider
		}
		if loaded.BypassMode {
			permissionMode = "bypassPermissions"
		}
	}
	if defProvider == "" {
		defProvider = provider.DefaultProvider
	}
	store.OnAppend = server.notify
	server.defaults = metaDefaults{
		Provider:       defProvider,
		Model:          strings.TrimSpace(cfg.DefaultModel),
		Effort:         strings.ToLower(strings.TrimSpace(cfg.DefaultEffort)),
		PermissionMode: permissionMode,
	}
	go server.runSchedules()
	return server, nil
}

// Store exposes the conversation store (tests, tooling).
func (s *Server) Store() *Store { return s.store }

// ErrClosing refuses a turn while the server shuts down.
var ErrClosing = errors.New("ccl serve is shutting down")

// Close stops what the server started: scheduled messages, subscription
// logins, terminals and turns in flight. A cancelled turn is still stored with
// its result, so Close waits for that until ctx ends.
func (s *Server) Close(ctx context.Context) error {
	s.closing.Store(true)
	s.logins.mu.Lock()
	for _, login := range s.logins.logins {
		if !login.Done {
			login.Cancelled = true
			login.cancel()
		}
	}
	s.logins.mu.Unlock()
	s.closeTerminals()
	for _, done := range s.timeline.CancelAll() {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Every route is registered under the prefix, so ServeMux's own redirects
	// (a missing trailing slash, an unclean path) keep it; each handler and the
	// guard then see the path they would see at the root.
	handle := func(pattern string, handler http.Handler) {
		mux.Handle(s.basePath+pattern, stripBasePath(s.basePath, s.guard(handler)))
	}
	handleFunc := func(pattern string, handler http.HandlerFunc) { handle(pattern, handler) }
	handleFunc("/health", s.handleHealth)
	handleFunc("/api/providers", s.authenticated(s.handleProviders))
	handleFunc("/api/config", s.authenticated(s.handleConfig))
	handleFunc("/api/config/providers", s.authenticated(s.handleConfigProviders))
	handleFunc("/api/config/providers/", s.authenticated(s.handleConfigProvider))
	handleFunc("/api/sessions", s.authenticated(s.handleSessions))
	handleFunc("/api/capabilities", s.authenticated(s.handleCapabilities))
	handleFunc("/api/workspaces", s.authenticated(s.handleWorkspaces))
	handleFunc("/api/workspaces/", s.authenticated(s.handleWorkspaces))
	handleFunc("/api/directories", s.authenticated(s.handleDirectories))
	handleFunc("/api/files/", s.authenticated(s.withWorkspace(s.handleFiles)))
	handleFunc("/api/git/", s.authenticated(s.withWorkspace(s.handleGit)))
	handleFunc("/api/claude-sessions", s.authenticated(s.withWorkspace(s.handleClaudeSessions)))
	handleFunc("/api/oauth/login", s.authenticated(s.handleOAuth))
	handleFunc("/api/oauth/login/", s.authenticated(s.handleOAuth))
	handleFunc("/api/mcp", s.authenticated(s.withWorkspace(s.handleMCP)))
	handleFunc("/api/mcp/", s.authenticated(s.withWorkspace(s.handleMCP)))
	handleFunc("/api/update", s.authenticated(s.handleUpdate))
	handleFunc("/api/memory", s.authenticated(s.handleMemory))
	handleFunc("/api/cloud", s.authenticated(s.handleCloud))
	handleFunc("/api/cloud/", s.authenticated(s.handleCloud))
	// A share link carries its own token instead of the server's.
	handleFunc("/api/share/", s.handleShared)
	// The terminal authenticates itself: its stream also takes ?token=.
	handleFunc("/api/terminal", s.handleTerminal)
	handleFunc("/api/terminal/", s.handleTerminal)
	handleFunc("/api/claude-sessions/", s.authenticated(s.withWorkspace(s.handleClaudeSessions)))
	// <img> cannot send a header: the raw file route also takes ?token=.
	handleFunc("/api/files/raw", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) && !s.queryAuthorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.withWorkspace(s.serveRaw)(w, r)
	})
	// The session routes authenticate themselves: the stream and events
	// endpoints also accept ?token=, which EventSource can use and a header
	// cannot.
	handleFunc("/api/sessions/", s.handleSession)
	handle("/", s.staticHandler())
	if s.basePath != "" {
		// A container probe keeps asking /health; nothing else answers
		// outside the prefix.
		mux.Handle("/health", s.guard(http.HandlerFunc(s.handleHealth)))
		mux.HandleFunc(s.basePath, func(w http.ResponseWriter, r *http.Request) {
			target := s.basePath + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
		})
	}
	return mux
}

func (s *Server) staticHandler() http.Handler {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(assets))
	etags := assetETags(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The page carries no secret; it asks for one and keeps it locally.
		// no-cache still lets the browser keep a copy: it revalidates every
		// load and gets a 304 while the binary is unchanged. web/ is the build
		// output of internal/webui/frontend (npm run build).
		w.Header().Set("Cache-Control", "no-cache")
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		// A share link is the page itself, which reads /api/share/<token>.
		if strings.HasPrefix(name, "share/") {
			w.Header().Set("Referrer-Policy", "no-referrer")
			name = "index.html"
		}
		// The page is served as rendered for this prefix, and validated by
		// what it is, not by the embedded template.
		if name == "index.html" {
			w.Header().Set("ETag", s.indexETag)
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.index))
			return
		}
		// Vite names built assets by content hash: one URL never changes.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if tag, ok := etags[name]; ok {
			w.Header().Set("ETag", tag)
			if etagMatches(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

// assetETags hashes every embedded asset. An embedded file has no modification
// time, so the content is the only thing a validator can come from.
func assetETags(assets fs.FS) map[string]string {
	etags := make(map[string]string)
	_ = fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			return nil
		}
		etags[name] = contentETag(data)
		return nil
	})
	return etags
}

func contentETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

// pageIndex is the embedded page with its base filled in for basePath. A
// build without the base slot still serves at the root.
func pageIndex(basePath string) ([]byte, error) {
	index, err := fs.ReadFile(webAssets, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("the web UI is not built: %w", err)
	}
	rendered, err := renderIndex(index, basePath)
	if err != nil && basePath == "" {
		return index, nil
	}
	return rendered, err
}

// etagMatches applies If-None-Match's weak comparison.
func etagMatches(header, tag string) bool {
	weak := strings.TrimPrefix(tag, "W/")
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == weak {
			return true
		}
	}
	return false
}

// ── authentication ──────────────────────────────────────────────────────────

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if values := r.Header.Values(PageTokenHeader); len(values) > 0 {
		return tokenEqual(strings.TrimSpace(values[0]), s.token)
	}
	header := strings.TrimSpace(r.Header.Get(TokenHeader))
	if after, ok := cutPrefixFold(header, "bearer "); ok {
		return tokenEqual(after, s.token)
	}
	// No usable header: tokenEqual accepts that only in loopback mode, where the
	// configured token is empty.
	return tokenEqual("", s.token)
}

// queryAuthorized is the EventSource path: browsers cannot set headers there.
func (s *Server) queryAuthorized(r *http.Request) bool {
	return tokenEqual(r.URL.Query().Get(TokenQuery), s.token)
}

func tokenEqual(got, want string) bool {
	if want == "" {
		// Local-only mode: the listener is loopback, so the browser is the user.
		return true
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func cutPrefixFold(text, prefix string) (string, bool) {
	if len(text) < len(prefix) || !strings.EqualFold(text[:len(prefix)], prefix) {
		return text, false
	}
	return strings.TrimSpace(text[len(prefix):]), true
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessions, _ := s.store.List()
	defaultWorkspace, _ := s.workspaces.fallback()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"version":       s.version,
		"claudeVersion": claudeVersion(),
		"sessions":      len(sessions),
		// workspace is the default one, for clients that know only one.
		"workspace":  defaultWorkspace.Path,
		"workspaces": s.workspaces.list(),
		// authRequired tells the page whether to ask for a token at all.
		"authRequired": s.token != "",
		"shell":        s.shell,
	})
}

// claudeVersion asks the installed Claude Code for its version. It is cached:
// /health may be polled by a container orchestration probe.
var claudeVersion = func() func() string {
	var once sync.Once
	var version string
	return func() string {
		once.Do(func() {
			path, err := exec.LookPath("claude")
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, path, "--version").Output()
			if err != nil {
				return
			}
			version = strings.TrimSpace(string(out))
		})
		return version
	}
}()

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load ccl config: "+err.Error())
		return
	}
	providers := []ProviderInfo{{
		Name:        provider.DefaultProvider,
		IsDefault:   true,
		Slots:       map[string]string{},
		EffortLevel: "",
		Active:      provider.IsDefaultProvider(cfg.ActiveProvider),
	}}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		p := cfg.Providers[name]
		providers = append(providers, ProviderInfo{
			Name:        name,
			Slots:       slotsOf(p),
			EffortLevel: strings.TrimSpace(p.EffortLevel),
			Active:      name == cfg.ActiveProvider,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "current": cfg.ActiveProvider})
}

func slotsOf(p provider.Provider) map[string]string {
	slots := map[string]string{
		"opus":     stripOneM(p.OpusModel),
		"sonnet":   stripOneM(p.SonnetModel),
		"haiku":    stripOneM(p.HaikuModel),
		"fable":    stripOneM(p.FableModel),
		"custom":   stripOneM(p.CustomModelID),
		"subagent": stripOneM(p.SubagentModel),
	}
	for key, value := range slots {
		if value == "" {
			delete(slots, key)
		}
	}
	return slots
}

func stripOneM(model string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(model), "[1m]"))
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions, err := s.store.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := make([]SessionView, 0, len(sessions))
		for _, meta := range sessions {
			views = append(views, s.view(meta))
		}
		writeJSON(w, http.StatusOK, views)

	case http.MethodPost:
		var body struct {
			Title    string `json:"title"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
			// Workspace is a workspace id; empty means the default one. A path
			// is never accepted: clients only choose among served directories.
			Workspace string `json:"workspace"`
			// Worktree runs the conversation on a new branch in a worktree.
			Worktree       bool   `json:"worktree"`
			PermissionMode string `json:"permissionMode"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		workspace, ok := s.workspaces.fallback()
		if body.Workspace != "" {
			workspace, ok = s.workspaces.byID(body.Workspace)
		}
		if !ok {
			writeError(w, http.StatusBadRequest, ErrWorkspaceNotServed.Error())
			return
		}
		meta := Meta{
			Title:     strings.TrimSpace(body.Title),
			Provider:  s.defaults.Provider,
			Model:     s.defaults.Model,
			Effort:    s.defaults.Effort,
			Workspace: workspace.Path,
		}
		// The request wins, then the project's defaults, then the server's.
		s.applyProjectDefaults(&meta, workspace.ID, strings.TrimSpace(body.Provider) != "", strings.TrimSpace(body.Model) != "", strings.TrimSpace(body.Effort) != "")
		if body.PermissionMode != "" {
			if !validPermissionMode(body.PermissionMode) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("permission mode: expected one of %s", strings.Join(PermissionModes, ", ")))
				return
			}
			meta.PermissionMode = body.PermissionMode
		}
		if strings.TrimSpace(body.Provider) != "" {
			meta.Provider = strings.TrimSpace(body.Provider)
		}
		if strings.TrimSpace(body.Model) != "" {
			meta.Model = strings.TrimSpace(body.Model)
		}
		if strings.TrimSpace(body.Effort) != "" {
			meta.Effort = strings.ToLower(strings.TrimSpace(body.Effort))
		}
		if err := s.validateProvider(meta.Provider); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateEffort(meta.Effort); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Worktree {
			meta.ID = uuid.NewString()
			info, err := s.createWorktree(r.Context(), workspace, meta.ID[:8])
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			meta.Worktree = &info
		}
		session, err := s.store.Create(meta)
		if err != nil {
			if meta.Worktree != nil {
				_ = s.removeWorktree(context.WithoutCancel(r.Context()), workspace, *meta.Worktree, true)
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, s.view(session.Meta()))

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// settleTimeout bounds how long a request waits for a finished turn's
// process to exit.
const settleTimeout = 15 * time.Second

// handleSession routes /api/sessions/{id}[/...].
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 3 || (len(parts) == 3 && action != "uploads" && action != "schedules" && action != "worktree") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// The stream, events and attachment downloads take ?token= as well:
	// EventSource and <img> cannot send a header. Every other route accepts
	// only the header.
	queryAuth := action == "stream" || action == "events" || (action == "uploads" && len(parts) == 3 && r.Method == http.MethodGet)
	if !s.authorized(r) && !(queryAuth && s.queryAuthorized(r)) {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	session, err := s.store.Open(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The browser ends a turn at its result, while the process may still be
	// exiting; a change right after it waits for that instead of a 409.
	if r.Method != http.MethodGet && action != "cancel" && action != "permissions" {
		s.timeline.Settle(session.ID(), settleTimeout)
	}

	switch action {
	case "":
		s.handleSessionMeta(w, r, session)
	case "messages":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessage(w, r, session)
	case "stream":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleStream(w, r, session)
	case "events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		since := parseSeq(r.URL.Query().Get("since"))
		s.syncMirror(session)
		events, err := session.Events(since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if events == nil {
			events = []Event{}
		}
		writeJSON(w, http.StatusOK, events)
	case "schedules":
		scheduleID := ""
		if len(parts) == 3 {
			scheduleID = parts[2]
		}
		s.handleSchedules(w, r, session, scheduleID)
	case "worktree":
		sub := ""
		if len(parts) == 3 {
			sub = parts[2]
		}
		s.handleWorktree(w, r, session, sub)
	case "share":
		s.handleShare(w, r, session)
	case "permissions":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handlePermission(w, r, session)
	case "uploads":
		if len(parts) == 3 {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			s.serveUpload(w, session, parts[2])
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleUploads(w, r, session)
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !s.timeline.Running(session.ID()) {
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": false, "running": false})
			return
		}
		s.timeline.RequestCancel(session.ID())
		writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": true, "running": true})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleSessionMeta(w http.ResponseWriter, r *http.Request, session *Session) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.view(session.Meta()))

	case http.MethodPatch:
		var body struct {
			Title    *string `json:"title"`
			Provider *string `json:"provider"`
			Model    *string `json:"model"`
			Effort   *string `json:"effort"`
			// PermissionMode is "" (the default) or one of PermissionModes.
			PermissionMode *string `json:"permissionMode"`
			Pinned         *bool   `json:"pinned"`
			Archived       *bool   `json:"archived"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// What the next turn launches with cannot change under a running
		// one; its title and where it is listed can.
		route := body.Provider != nil || body.Model != nil || body.Effort != nil || body.PermissionMode != nil
		if route && s.timeline.Running(session.ID()) {
			writeError(w, http.StatusConflict, ErrBusy.Error())
			return
		}
		if !route && body.Title == nil {
			meta, err := session.RelabelMeta(func(meta *Meta) {
				if body.Pinned != nil {
					meta.Pinned = *body.Pinned
				}
				if body.Archived != nil {
					meta.Archived = *body.Archived
				}
			})
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, s.view(meta))
			return
		}
		if body.PermissionMode != nil && *body.PermissionMode != "" && !validPermissionMode(*body.PermissionMode) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("permission mode: expected one of %s", strings.Join(PermissionModes, ", ")))
			return
		}
		if body.Provider != nil {
			if err := s.validateProvider(*body.Provider); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if body.Effort != nil {
			if err := validateEffort(strings.ToLower(strings.TrimSpace(*body.Effort))); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		meta, err := session.UpdateMeta(func(meta *Meta) {
			if body.Title != nil {
				meta.Title = strings.TrimSpace(*body.Title)
			}
			if body.Provider != nil {
				meta.Provider = strings.TrimSpace(*body.Provider)
			}
			if body.Model != nil {
				meta.Model = strings.TrimSpace(*body.Model)
			}
			if body.Effort != nil {
				meta.Effort = strings.ToLower(strings.TrimSpace(*body.Effort))
			}
			if body.PermissionMode != nil {
				meta.PermissionMode = *body.PermissionMode
			}
			if body.Pinned != nil {
				meta.Pinned = *body.Pinned
			}
			if body.Archived != nil {
				meta.Archived = *body.Archived
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.view(meta))

	case http.MethodDelete:
		if s.timeline.Running(session.ID()) {
			writeError(w, http.StatusConflict, ErrBusy.Error())
			return
		}
		// A worktree goes with its conversation; uncommitted work in it
		// refuses the delete unless ?force=1. An unmerged branch is kept.
		if meta := session.Meta(); meta.Worktree != nil {
			if workspace, ok := s.workspaces.byPath(meta.Workspace); ok {
				if err := s.removeWorktree(r.Context(), workspace, *meta.Worktree, r.URL.Query().Get("force") == "1"); err != nil {
					writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "worktreeDirty": true})
					return
				}
			}
		}
		if err := s.store.Delete(session.ID()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, session *Session) {
	var body struct {
		Text     string `json:"text"`
		Effort   string `json:"effort"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
		// Attachments are ids returned by the uploads endpoint.
		Attachments []string `json:"attachments"`
		// Steer sends the message into the running turn, if there is one,
		// instead of being refused as busy.
		Steer bool `json:"steer"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	attachments, err := resolveAttachments(session, body.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if text == "" && len(attachments) == 0 {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	// A turn's provider/model/effort take effect at the process boundary, so a
	// change arrives with the message that needs it; startTurn reads it back.
	if changed := strings.TrimSpace(body.Provider) != "" || strings.TrimSpace(body.Model) != "" || strings.TrimSpace(body.Effort) != ""; changed {
		if s.timeline.Running(session.ID()) {
			writeError(w, http.StatusConflict, ErrBusy.Error())
			return
		}
		if providerName := strings.TrimSpace(body.Provider); providerName != "" {
			if err := s.validateProvider(providerName); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		effort := strings.ToLower(strings.TrimSpace(body.Effort))
		if effort != "" {
			if err := validateEffort(effort); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		_, err := session.UpdateMeta(func(meta *Meta) {
			if providerName := strings.TrimSpace(body.Provider); providerName != "" {
				meta.Provider = providerName
			}
			if model := strings.TrimSpace(body.Model); model != "" {
				meta.Model = model
			}
			if effort != "" {
				meta.Effort = effort
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if body.Steer && s.timeline.Running(session.ID()) {
		spec := LaunchSpec{Prompt: text, Attachments: launchAttachments(session, attachments), attachmentViews: attachments}
		err := s.timeline.Steer(session.ID(), spec)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]any{"steered": true, "running": true})
			return
		case errors.Is(err, errTurnEnded):
			// The turn ended meanwhile: the message starts the next one.
			s.timeline.Settle(session.ID(), settleTimeout)
		case errors.Is(err, ErrBusy):
			writeError(w, http.StatusConflict, err.Error())
			return
		default:
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if _, err := s.startTurn(session, text, attachments); err != nil {
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrWorkspaceNotServed) || errors.Is(err, ErrWorktreeMissing) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, ErrClosing) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if isLaunchStart(err) {
			// The failure is already in the conversation history.
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"messageId": session.LastSeq(), "seq": session.LastSeq(), "running": true})
}

// startTurn runs one turn of a conversation with its current settings. Both
// the message endpoint and the scheduler start turns through it.
func (s *Server) startTurn(session *Session, text string, attachments []Attachment) (*TurnHandle, error) {
	meta := session.Meta()
	if meta.Title == "" || meta.Title == "New conversation" {
		title := text
		if title == "" && len(attachments) > 0 {
			title = attachments[0].Name
		}
		_, _ = session.UpdateMeta(func(meta *Meta) { meta.Title = TitleFrom(title) })
	}

	spec := LaunchSpec{
		Provider:     meta.Provider,
		Subscription: s.isSubscription(meta.Provider),
		Model:        meta.Model,
		Effort:       meta.Effort,
		Workspace:    meta.Workspace,
		Resume:       meta.ClaudeSessionID,
		Prompt:       text,
		// An empty mode follows the server default.
		PermissionMode: firstNonEmpty(meta.PermissionMode, s.defaults.PermissionMode),
	}
	spec.Attachments = launchAttachments(session, attachments)
	spec.attachmentViews = attachments
	// A conversation runs where it started. One whose directory is no longer
	// served is refused, never moved: its Claude session belongs there.
	if s.closing.Load() {
		return nil, ErrClosing
	}
	workspace, ok := s.workspaces.byPath(meta.Workspace)
	if meta.Workspace == "" {
		workspace, ok = s.workspaces.fallback()
	}
	if !ok {
		return nil, ErrWorkspaceNotServed
	}
	spec.Workspace = workspace.Path
	if meta.Worktree != nil {
		tree, err := s.worktreeWorkspace(workspace, meta.Worktree.Name)
		if err != nil {
			return nil, err
		}
		spec.Workspace = tree.Path
	}
	s.pauseMirror(session)
	handle, err := s.timeline.Start(s.runner, session, spec)
	if err != nil {
		return nil, err
	}
	go s.notifyOnDone(session.ID(), handle.Done)
	return handle, nil
}

// launchAttachments is how a message's uploads reach Claude Code.
func launchAttachments(session *Session, attachments []Attachment) []LaunchAttachment {
	var out []LaunchAttachment
	for _, attachment := range attachments {
		out = append(out, LaunchAttachment{
			Path:      session.AttachmentPath(attachment.ID),
			Name:      attachment.Name,
			MediaType: attachment.Type,
			Image:     attachment.Image,
		})
	}
	return out
}

// isSubscription reports whether a provider logs in to a subscription
// (OAuth) rather than paying per token. Claude Code's own configuration is
// not known either way, so it is not one.
func (s *Server) isSubscription(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || provider.IsDefaultProvider(name) {
		return false
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return strings.TrimSpace(cfg.Providers[name].OAuthProvider) != ""
}

func (s *Server) validateProvider(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if provider.IsDefaultProvider(name) {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	if _, ok := cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q not found; use ccl ls or ccl set", name)
	}
	return nil
}

func validateEffort(level string) error {
	level = strings.TrimSpace(level)
	if level == "" {
		return nil
	}
	for _, candidate := range EffortLevels {
		if candidate == level {
			return nil
		}
	}
	return fmt.Errorf("effort: expected one of %s, got %q", strings.Join(EffortLevels, ", "), level)
}

// withinWorkspace reports whether dir is the workspace or below it.
func withinWorkspace(dir, workspace string) bool {
	rel, err := filepath.Rel(workspace, dir)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// handleCapabilities reports the slash commands and MCP servers Claude Code
// listed in the most recent turn of any conversation, so a new conversation's
// menus are not empty. Each turn reports its own in its init event.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	init := s.runner.Capabilities()
	if init.SlashCommands == nil {
		init.SlashCommands = []string{}
	}
	if init.MCPServers == nil {
		init.MCPServers = []MCPServer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slashCommands":         init.SlashCommands,
		"mcpServers":            init.MCPServers,
		"defaultPermissionMode": s.defaults.PermissionMode,
	})
}

// ── SSE ─────────────────────────────────────────────────────────────────────

// heartbeatInterval keeps proxies from closing an idle stream.
const heartbeatInterval = 15 * time.Second

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, session *Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	since := parseSeq(r.URL.Query().Get("since"))
	if since == 0 {
		// EventSource sends Last-Event-ID on reconnect.
		since = parseSeq(r.Header.Get("Last-Event-ID"))
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	changed := s.subscribe(session.ID())
	defer s.unsubscribe(session.ID(), changed)
	// What a terminal adds to the conversation's Claude session shows too.
	go s.followTranscript(r.Context(), session)

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		events, err := session.Events(since)
		if err != nil {
			s.writeEvent(w, flusher, Event{Type: EventError, Payload: mustJSON(ErrorPayload{Message: err.Error()})})
			return
		}
		for _, event := range events {
			if !s.writeEvent(w, flusher, event) {
				return
			}
			since = event.Seq
		}

		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) writeEvent(w http.ResponseWriter, flusher http.Flusher, event Event) bool {
	if _, err := fmt.Fprintf(w, "id: %d\n", event.Seq); err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return false
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	// A JSON event payload never contains a raw newline, so one data line is
	// enough; json.Marshal escapes them inside strings.
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (s *Server) subscribe(id string) chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers[id] = append(s.subscribers[id], ch)
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(id string, target chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.subscribers[id]
	for i, ch := range list {
		if ch == target {
			s.subscribers[id] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.subscribers[id]) == 0 {
		delete(s.subscribers, id)
	}
}

// Publish wakes the streams watching a session. Callers use it after appending
// events themselves; the turn runner publishes per event.
func (s *Server) Publish(id string) { s.notify(id) }

func (s *Server) notify(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subscribers[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) notifyOnDone(id string, done <-chan struct{}) {
	<-done
	s.notify(id)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func parseSeq(value string) int64 {
	var seq int64
	for _, char := range strings.TrimSpace(value) {
		if char < '0' || char > '9' {
			return 0
		}
		seq = seq*10 + int64(char-'0')
		if seq < 0 {
			return 0
		}
	}
	return seq
}

func decodeBody(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("ccl serve: write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func isLaunchStart(err error) bool {
	message := err.Error()
	return strings.Contains(message, "start Claude Code") || strings.Contains(message, "not found in PATH")
}
