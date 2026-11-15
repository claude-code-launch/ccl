package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/google/uuid"
)

// Subscription logins, MCP servers and the update check: settings the page
// changes through the same programs a terminal user would run.

// ── subscription login ──────────────────────────────────────────────────────

// OAuthProviders are the names `ccl oauth` accepts.
var OAuthProviders = []string{"gpt", "gemini", "grok", "qoder", "kimi", "kiro", "workbuddy", "workbuddycn", "autoclaw", "cursor"}

var (
	aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	urlPattern   = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

const loginTimeout = 10 * time.Minute

type oauthLogin struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Output   []string `json:"output"`
	URLs     []string `json:"urls"`
	Done     bool     `json:"done"`
	OK       bool     `json:"ok"`
	// Cancelled marks a login the user stopped, which is not a failure.
	Cancelled bool   `json:"cancelled,omitempty"`
	Error     string `json:"error,omitempty"`
	// stdin takes a pasted redirect address (cloud login); nil otherwise.
	stdin io.WriteCloser

	cancel context.CancelFunc
}

type loginRegistry struct {
	mu     sync.Mutex
	logins map[string]*oauthLogin
}

// handleOAuth runs `ccl oauth <provider> --no-browser` and relays what it
// prints: the URL to open (and the code to enter, for device logins). The
// login itself, its callback and the provider it creates are ccl oauth's.
func (s *Server) handleOAuth(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/oauth/login"), "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		s.startLogin(w, r)
	case id != "" && r.Method == http.MethodGet:
		s.logins.mu.Lock()
		login, ok := s.logins.logins[id]
		var view oauthLogin
		if ok {
			view = *login
			view.Output = append([]string(nil), login.Output...)
			view.URLs = append([]string(nil), login.URLs...)
		}
		s.logins.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "no such login")
			return
		}
		writeJSON(w, http.StatusOK, view)
	case id != "" && r.Method == http.MethodDelete:
		s.logins.mu.Lock()
		login, ok := s.logins.logins[id]
		if ok && !login.Done {
			login.Cancelled = true
		}
		s.logins.mu.Unlock()
		if ok && login.cancel != nil {
			login.cancel()
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Alias    string `json:"alias"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	known := false
	for _, name := range OAuthProviders {
		if name == body.Provider {
			known = true
		}
	}
	if !known {
		writeError(w, http.StatusBadRequest, "unknown subscription "+body.Provider)
		return
	}
	alias := strings.TrimSpace(body.Alias)
	if alias != "" && !aliasPattern.MatchString(alias) {
		writeError(w, http.StatusBadRequest, "invalid provider name")
		return
	}
	if s.selfBinary == "" {
		writeError(w, http.StatusNotImplemented, "subscription login is not available in this build")
		return
	}
	args := []string{"oauth", body.Provider}
	if alias != "" {
		args = append(args, alias)
	}
	args = append(args, "--no-browser")
	login, err := s.runLogin(body.Provider, args, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": login.ID})
}

// runLogin starts `ccl <args>` and keeps what it prints, with the URLs in it,
// for the page to poll. withStdin keeps its input open for a pasted line.
func (s *Server) runLogin(provider string, args []string, withStdin bool) (*oauthLogin, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	cmd := exec.CommandContext(ctx, s.selfBinary, args...)
	if workspace, ok := s.workspaces.fallback(); ok {
		cmd.Dir = workspace.Path
	}
	cmd.Stdin = nil
	var stdin io.WriteCloser
	if withStdin {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		stdin = pipe
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	login := &oauthLogin{ID: uuid.NewString(), Provider: provider, Output: []string{}, URLs: []string{}, cancel: cancel, stdin: stdin}
	s.logins.mu.Lock()
	s.logins.logins[login.ID] = login
	s.logins.mu.Unlock()

	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := redactSecrets(strings.TrimRight(scanner.Text(), "\r"))
			s.logins.mu.Lock()
			if len(login.Output) < 200 {
				login.Output = append(login.Output, line)
			}
			login.URLs = append(login.URLs, urlPattern.FindAllString(line, -1)...)
			s.logins.mu.Unlock()
		}
	}()
	go func() {
		err := cmd.Wait()
		_ = writer.Close()
		<-scanned
		cancel()
		s.logins.mu.Lock()
		login.Done = true
		login.OK = err == nil
		if err != nil && !login.Cancelled {
			login.Error = lastLine(strings.Join(login.Output, "\n"), err.Error())
		}
		s.logins.mu.Unlock()
	}()
	return login, nil
}

// ── MCP servers ─────────────────────────────────────────────────────────────

// MCPEntry is one configured MCP server. Environment and header values are
// never shown: they usually carry keys.
type MCPEntry struct {
	Name   string   `json:"name"`
	Scope  string   `json:"scope"` // user or project
	Type   string   `json:"type"`
	Target string   `json:"target"`
	Env    []string `json:"env,omitempty"` // names only
}

// The first character is never "-": the name is an argument to grok mcp.
var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request, ws Workspace) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/mcp"), "/")
	switch {
	case name == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"servers": mcpServers(r.Context(), ws), "grok": grokOnPath()})
	case name == "" && r.Method == http.MethodPost:
		s.addMCP(w, r, ws)
	case name != "" && r.Method == http.MethodDelete:
		s.removeMCP(w, r, ws, name)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func grokOnPath() bool {
	_, err := groklaunch.LookPath()
	return err == nil
}

func mcpServers(ctx context.Context, ws Workspace) []MCPEntry {
	entries := []MCPEntry{}
	seen := map[string]bool{}
	appendEntry := func(entry MCPEntry) {
		key := entry.Scope + "\x00" + entry.Name
		if entry.Name == "" || seen[key] {
			return
		}
		seen[key] = true
		sort.Strings(entry.Env)
		entries = append(entries, entry)
	}

	if grokPath, err := groklaunch.LookPath(); err == nil {
		listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(listCtx, grokPath, "mcp", "list", "--json")
		cmd.Dir = ws.real
		cmd.Env = cleanGrokCLISubcommandEnv(os.Environ())
		out, err := cmd.Output()
		cancel()
		if err == nil {
			if idx := strings.IndexByte(string(out), '['); idx >= 0 {
				var listed []struct {
					Name      string            `json:"name"`
					Scope     string            `json:"scope"`
					Transport string            `json:"transport"`
					Type      string            `json:"type"`
					Command   string            `json:"command"`
					Args      []string          `json:"args"`
					URL       string            `json:"url"`
					Env       map[string]string `json:"env"`
				}
				if json.Unmarshal(out[idx:], &listed) == nil {
					for _, srv := range listed {
						kind := firstNonEmpty(srv.Transport, srv.Type)
						if kind == "" {
							if srv.URL != "" {
								kind = "http"
							} else {
								kind = "stdio"
							}
						}
						scope := srv.Scope
						if scope == "" {
							scope = "user"
						}
						entry := MCPEntry{Name: srv.Name, Scope: scope, Type: kind}
						if srv.URL != "" {
							entry.Target = srv.URL
						} else {
							entry.Target = redactSecrets(strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " ")))
						}
						for key := range srv.Env {
							entry.Env = append(entry.Env, key)
						}
						appendEntry(entry)
					}
				}
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(ws.real, ".mcp.json")); err == nil {
		var config struct {
			MCPServers map[string]struct {
				Type    string            `json:"type"`
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				URL     string            `json:"url"`
				Env     map[string]string `json:"env"`
			} `json:"mcpServers"`
		}
		if json.Unmarshal(data, &config) == nil {
			for name, srv := range config.MCPServers {
				kind := srv.Type
				if kind == "" {
					if srv.URL != "" {
						kind = "http"
					} else {
						kind = "stdio"
					}
				}
				entry := MCPEntry{Name: name, Scope: "project", Type: kind}
				if srv.URL != "" {
					entry.Target = srv.URL
				} else {
					entry.Target = redactSecrets(strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " ")))
				}
				for key := range srv.Env {
					entry.Env = append(entry.Env, key)
				}
				appendEntry(entry)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Scope != entries[j].Scope {
			return entries[i].Scope < entries[j].Scope
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func normalizeMCPScope(scope string) (string, bool) {
	switch scope {
	case "user", "project":
		return scope, true
	case "local":
		return "project", true
	default:
		return "", false
	}
}

// addMCP runs `grok mcp add`, storing the server in Grok Build's own user or
// project configuration.
func (s *Server) addMCP(w http.ResponseWriter, r *http.Request, ws Workspace) {
	var body struct {
		Name    string            `json:"name"`
		Scope   string            `json:"scope"`
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		URL     string            `json:"url"`
		Env     map[string]string `json:"env"`
		Headers map[string]string `json:"headers"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope, ok := normalizeMCPScope(body.Scope)
	if !mcpNamePattern.MatchString(body.Name) || !ok {
		writeError(w, http.StatusBadRequest, "a name of letters, digits, - or _ and a scope of user or project are required")
		return
	}

	args := []string{"add", "--scope", scope}
	switch body.Type {
	case "", "stdio":
		cmdName := strings.TrimSpace(body.Command)
		if cmdName == "" {
			writeError(w, http.StatusBadRequest, "a stdio server needs a command")
			return
		}
		args = append(args, "--transport", "stdio")
		envKeys := make([]string, 0, len(body.Env))
		for k := range body.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			args = append(args, "-e", k+"="+body.Env[k])
		}
		args = append(args, body.Name, "--", cmdName)
		args = append(args, body.Args...)

	case "http", "sse":
		if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
			writeError(w, http.StatusBadRequest, "an http or sse server needs an http(s) URL")
			return
		}
		args = append(args, "--transport", body.Type)
		headerKeys := make([]string, 0, len(body.Headers))
		for k := range body.Headers {
			headerKeys = append(headerKeys, k)
		}
		sort.Strings(headerKeys)
		for _, k := range headerKeys {
			args = append(args, "-H", k+": "+body.Headers[k])
		}
		args = append(args, body.Name, body.URL)

	default:
		writeError(w, http.StatusBadRequest, "type must be stdio, http or sse")
		return
	}

	s.runGrokMCP(w, r, ws, args...)
}

func (s *Server) removeMCP(w http.ResponseWriter, r *http.Request, ws Workspace, name string) {
	scope, ok := normalizeMCPScope(r.URL.Query().Get("scope"))
	if !mcpNamePattern.MatchString(name) || !ok {
		writeError(w, http.StatusBadRequest, "invalid name or scope")
		return
	}
	s.runGrokMCP(w, r, ws, "remove", "--scope", scope, name)
}

func cleanGrokCLISubcommandEnv(inherited []string) []string {
	env := make([]string, 0, len(inherited)+1)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		switch strings.ToUpper(key) {
		case "GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_DISABLE_AUTOUPDATER":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GROK_DISABLE_AUTOUPDATER=1")
}

func (s *Server) runGrokMCP(w http.ResponseWriter, r *http.Request, ws Workspace, args ...string) {
	grokPath, err := groklaunch.LookPath()
	if err != nil {
		writeError(w, http.StatusNotImplemented, "grok is not on PATH")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, grokPath, append([]string{"mcp"}, args...)...)
	cmd.Dir = ws.real
	cmd.Env = cleanGrokCLISubcommandEnv(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		writeError(w, http.StatusBadRequest, firstNonEmpty(redactSecrets(strings.TrimSpace(string(output))), err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": mcpServers(r.Context(), ws)})
}

// ── update check ────────────────────────────────────────────────────────────

type updateCache struct {
	mu      sync.Mutex
	checked time.Time
	latest  string
}

const updateCheckInterval = 6 * time.Hour

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	latest := ""
	if s.latestVersion != nil {
		s.update.mu.Lock()
		if time.Since(s.update.checked) > updateCheckInterval {
			s.update.checked = time.Now()
			if version, err := s.latestVersion(); err == nil {
				s.update.latest = version
			}
		}
		latest = s.update.latest
		s.update.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": s.version,
		"latest":  latest,
		"newer":   newerVersion(latest, s.version),
	})
}

// newerVersion compares vMAJOR.MINOR.PATCH tags; a development build is never
// behind.
func newerVersion(latest, current string) bool {
	parse := func(version string) ([3]int, bool) {
		var parts [3]int
		_, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(version), "v"), "%d.%d.%d", &parts[0], &parts[1], &parts[2])
		return parts, err == nil
	}
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}
