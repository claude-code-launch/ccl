// Package webui serves local Grok Build conversations over an authenticated HTTP listener.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/google/uuid"
)

//go:embed assets/*
var assets embed.FS

type Options struct {
	GrokPath   string
	StateDir   string
	Provider   provider.Provider
	Workspaces []string
	Port       int
}

type server struct {
	opts    Options
	host    string
	token   string
	ctx     context.Context
	mu      sync.Mutex
	chats   map[string]*conversation
	closing bool
	workers sync.WaitGroup
	slots   chan struct{}
}

// Serve runs until ctx is cancelled. ready receives a URL whose fragment
// contains the browser's separate, per-server credential.
func Serve(ctx context.Context, opts Options, ready func(string)) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	app, release, err := newServer(ctx, opts, listener.Addr().String())
	if err != nil {
		return err
	}
	defer release()
	service := &http.Server{
		Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: time.Minute, WriteTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- service.Serve(listener) }()
	if ready != nil {
		ready("http://" + app.host + "/#" + app.token)
	}
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	app.mu.Lock()
	app.closing = true
	app.mu.Unlock()
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if service.Shutdown(shutdownCtx) != nil {
		_ = service.Close()
	}
	app.workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newServer(ctx context.Context, opts Options, host string) (*server, func(), error) {
	var workspaces []string
	for _, path := range opts.Workspaces {
		resolved, err := resolveWorkspace(path)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace %q: %w", path, err)
		}
		if !slices.Contains(workspaces, resolved) {
			workspaces = append(workspaces, resolved)
		}
	}
	if len(workspaces) == 0 {
		return nil, nil, errors.New("at least one workspace is required")
	}
	opts.Workspaces = workspaces
	if err := os.MkdirAll(filepath.Join(opts.StateDir, "sessions"), 0o700); err != nil {
		return nil, nil, err
	}
	release, ok, err := fsutil.TryFileLock(filepath.Join(opts.StateDir, "server.lock"))
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errors.New("another ccl serve process owns this browser profile")
	}
	s := &server{
		opts: opts, host: host, token: rand.Text(), ctx: ctx,
		chats: make(map[string]*conversation), slots: make(chan struct{}, 4),
	}
	if err := s.loadRecords(); err != nil {
		release()
		return nil, nil, err
	}
	return s, release, nil
}

func resolveWorkspace(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("expected a directory")
	}
	return resolved, nil
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", static("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.css", static("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.js", static("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /api/sessions", s.list)
	mux.HandleFunc("POST /api/sessions", s.create)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.stream)
	mux.HandleFunc("POST /api/sessions/{id}/prompt", s.prompt)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/sessions/{id}/permissions/{permission}", s.decide)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		origin := r.Header.Get("Origin")
		if r.Host != s.host || (origin != "" && origin != "http://"+s.host) {
			http.Error(w, "Invalid host or origin", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
			http.Error(w, "Open the session link printed by ccl serve.", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func static(name, mediaType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mediaType)
		_, _ = w.Write(data)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		http.Error(w, "Expected application/json", http.StatusUnsupportedMediaType)
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	chats := make([]summary, 0, len(s.chats))
	for _, chat := range s.chats {
		chat.mu.Lock()
		chats = append(chats, chat.record.summary)
		chat.mu.Unlock()
	}
	s.mu.Unlock()
	sort.Slice(chats, func(i, j int) bool { return chats[i].Updated.After(chats[j].Updated) })
	writeJSON(w, http.StatusOK, struct {
		Provider   string    `json:"provider"`
		Workspaces []string  `json:"workspaces"`
		Sessions   []summary `json:"sessions"`
	}{s.opts.Provider.Name, s.opts.Workspaces, chats})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !slices.Contains(s.opts.Workspaces, body.Workspace) {
		http.Error(w, "Workspace is not enabled for this server", http.StatusBadRequest)
		return
	}
	rec := record{
		summary:  summary{ID: uuid.NewString(), Workspace: body.Workspace, Title: "New session", Updated: time.Now()},
		Provider: s.opts.Provider.Name,
	}
	if err := s.save(rec); err != nil {
		http.Error(w, "Could not save the session", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.chats[rec.ID] = newConversation(rec)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, rec.summary)
}

func (s *server) lookup(w http.ResponseWriter, r *http.Request) *conversation {
	s.mu.Lock()
	chat := s.chats[r.PathValue("id")]
	s.mu.Unlock()
	if chat == nil {
		http.NotFound(w, r)
	}
	return chat
}

func (s *server) prompt(w http.ResponseWriter, r *http.Request) {
	chat := s.lookup(w, r)
	if chat == nil {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" || len(body.Text) > 64<<10 {
		http.Error(w, "Enter a prompt of up to 64 KiB", http.StatusBadRequest)
		return
	}
	s.startOperation(w, chat, body.Text)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	chat := s.lookup(w, r)
	if chat == nil {
		return
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if chat.active != nil && chat.active.ctx.Err() == nil {
		chat.active.cancel()
		chat.emitLocked(event{Kind: "state", Text: "stopping"})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	chat := s.lookup(w, r)
	if chat == nil {
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Decision != "allow" && body.Decision != "deny" {
		http.Error(w, "Choose allow or deny", http.StatusBadRequest)
		return
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	id := r.PathValue("permission")
	pending, ok := chat.pending[id]
	if !ok || chat.active == nil || chat.active.ctx.Err() != nil || (body.Decision == "allow" && !pending.canAllow) {
		http.Error(w, "Permission choice is no longer available", http.StatusConflict)
		return
	}
	delete(chat.pending, id)
	pending.decision <- body.Decision
	chat.emitLocked(event{Kind: "permission_closed", ID: id, Text: body.Decision})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	chat := s.lookup(w, r)
	if chat == nil {
		return
	}
	if !s.startOperation(w, chat, "") {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(w)
	write := func(message string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := io.WriteString(w, message)
		if err != nil || controller.Flush() != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Time{})
		return true
	}
	if !write(": connected\n\n") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	cursor := 0
	for {
		chat.mu.Lock()
		end := min(cursor+128, len(chat.events))
		batch := make([]event, 0, end-cursor)
		for _, e := range chat.events[cursor:end] {
			if e.Kind == "permission" {
				if _, ok := chat.pending[e.ID]; !ok {
					continue
				}
			}
			batch = append(batch, e)
		}
		changed := chat.changed
		chat.mu.Unlock()
		if end > cursor {
			cursor = end
			if len(batch) > 0 {
				data, _ := json.Marshal(batch)
				if !write("data: " + string(data) + "\n\n") {
					return
				}
			}
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
