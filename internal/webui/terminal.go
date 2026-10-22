//go:build !windows

package webui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/creack/pty"
	"github.com/google/uuid"
)

// The web terminal: a shell on a pseudo-terminal in the workspace. It is off
// unless ccl web runs with --shell. Output streams over SSE from a byte offset,
// so a reconnecting page resumes where it was; keystrokes arrive by POST.

const (
	terminalBuffer = 256 << 10
	maxTerminals   = 4
)

type terminal struct {
	id  string
	cmd *exec.Cmd
	pty *os.File

	mu      sync.Mutex
	buf     []byte
	base    int64 // offset of buf[0] in the whole output
	subs    []chan struct{}
	exited  bool
	created time.Time
}

type terminalRegistry struct {
	mu        sync.Mutex
	terminals map[string]*terminal
}

func (t *terminal) notify() {
	for _, ch := range t.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (t *terminal) pump() {
	chunk := make([]byte, 16<<10)
	for {
		n, err := t.pty.Read(chunk)
		if n > 0 {
			t.mu.Lock()
			t.buf = append(t.buf, chunk[:n]...)
			if over := len(t.buf) - terminalBuffer; over > 0 {
				t.buf = append([]byte(nil), t.buf[over:]...)
				t.base += int64(over)
			}
			t.notify()
			t.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	_ = t.cmd.Wait()
	t.mu.Lock()
	t.exited = true
	t.notify()
	t.mu.Unlock()
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/terminal"), "/")
	parts := strings.Split(rest, "/")
	stream := len(parts) == 2 && parts[1] == "stream" && r.Method == http.MethodGet
	// The stream is an EventSource, which cannot send a header.
	if !s.authorized(r) && !(stream && s.queryAuthorized(r)) {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	if !s.shell {
		writeError(w, http.StatusForbidden, "the terminal is off: start ccl web with --shell")
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodPost:
			s.openTerminal(w, r)
		case http.MethodGet:
			s.terminals.mu.Lock()
			ids := []string{}
			for id, term := range s.terminals.terminals {
				term.mu.Lock()
				if !term.exited {
					ids = append(ids, id)
				}
				term.mu.Unlock()
			}
			s.terminals.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"terminals": ids})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	s.terminals.mu.Lock()
	term, ok := s.terminals.terminals[parts[0]]
	s.terminals.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no such terminal")
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case stream:
		s.streamTerminal(w, r, term)
	case action == "input" && r.Method == http.MethodPost:
		var body struct {
			Data string `json:"data"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := io.WriteString(term.pty, body.Data); err != nil {
			writeError(w, http.StatusGone, "the terminal has exited")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "resize" && r.Method == http.MethodPost:
		var body struct {
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if err := decodeBody(r, &body); err != nil || body.Cols == 0 || body.Rows == 0 {
			writeError(w, http.StatusBadRequest, "cols and rows are required")
			return
		}
		_ = pty.Setsize(term.pty, &pty.Winsize{Cols: body.Cols, Rows: body.Rows})
		w.WriteHeader(http.StatusNoContent)
	case action == "" && r.Method == http.MethodDelete:
		s.closeTerminal(term)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) openTerminal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	_ = decodeBody(r, &body)
	workspace, err := s.workspaceFor(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if body.Cols == 0 || body.Rows == 0 {
		body.Cols, body.Rows = 100, 30
	}
	s.terminals.mu.Lock()
	live := 0
	for _, term := range s.terminals.terminals {
		term.mu.Lock()
		if !term.exited {
			live++
		}
		term.mu.Unlock()
	}
	s.terminals.mu.Unlock()
	if live >= maxTerminals {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d terminals at a time", maxTerminals))
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = workspace.Path
	cmd.Env = append(terminalEnv(), "TERM=xterm-256color", "COLORTERM=truecolor")
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: body.Cols, Rows: body.Rows})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start shell: "+err.Error())
		return
	}
	term := &terminal{id: uuid.NewString(), cmd: cmd, pty: file, created: time.Now()}
	s.terminals.mu.Lock()
	s.terminals.terminals[term.id] = term
	s.terminals.mu.Unlock()
	go term.pump()
	writeJSON(w, http.StatusCreated, map[string]string{"id": term.id})
}

// terminalEnv is the server's environment without ccl web's own credential.
func terminalEnv() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "CCL_WEB_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (s *Server) closeTerminal(term *terminal) {
	_ = term.pty.Close()
	if term.cmd.Process != nil {
		process.KillTree(term.cmd)
	}
	s.terminals.mu.Lock()
	delete(s.terminals.terminals, term.id)
	s.terminals.mu.Unlock()
}

func (s *Server) streamTerminal(w http.ResponseWriter, r *http.Request, term *terminal) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	since := parseSeq(r.URL.Query().Get("since"))
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	changed := make(chan struct{}, 1)
	term.mu.Lock()
	term.subs = append(term.subs, changed)
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		for i, ch := range term.subs {
			if ch == changed {
				term.subs = append(term.subs[:i], term.subs[i+1:]...)
				break
			}
		}
		term.mu.Unlock()
	}()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		term.mu.Lock()
		start := since - term.base
		if start < 0 {
			start = 0 // older output fell out of the buffer
		}
		var chunk []byte
		if start < int64(len(term.buf)) {
			chunk = append([]byte(nil), term.buf[start:]...)
		}
		end := term.base + int64(len(term.buf))
		exited := term.exited
		term.mu.Unlock()

		if len(chunk) > 0 {
			payload, _ := json.Marshal(map[string]any{"offset": end, "data": base64.StdEncoding.EncodeToString(chunk)})
			if _, err := fmt.Fprintf(w, "event: output\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			since = end
		}
		if exited {
			_, _ = io.WriteString(w, "event: exit\ndata: {}\n\n")
			flusher.Flush()
			return
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
