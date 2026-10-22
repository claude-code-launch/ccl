package webui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Native Claude Code sessions: the transcripts Claude Code keeps for the
// workspace under ~/.claude/projects. The page lists them and imports one into
// a web conversation that resumes it.

// ClaudeSession is one native transcript.
type ClaudeSession struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	Size    int64     `json:"size"`
	Prompts int       `json:"prompts"`
	// WebSession is the conversation that already resumes it, if any.
	WebSession string `json:"webSession,omitempty"`
}

var (
	claudeSessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)
	nonAlphanumeric      = regexp.MustCompile(`[^A-Za-z0-9]`)
	commandName          = regexp.MustCompile(`<command-name>\s*(/?[^<\s]+)\s*</command-name>`)
	commandArgs          = regexp.MustCompile(`<command-args>([\s\S]*?)</command-args>`)
)

const claudeListScanLines = 400

// claudeProjectDirs are the directories Claude Code may have used for the
// workspace: it names them after the working directory with every
// non-alphanumeric character replaced, and the process may report either the
// path as given or with links resolved.
func (s *Server) claudeProjectDirs() []string {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		root = filepath.Join(home, ".claude")
	}
	seen := map[string]bool{}
	var dirs []string
	for _, workspace := range []string{s.workspace, s.realWorkspace} {
		dir := filepath.Join(root, "projects", nonAlphanumeric.ReplaceAllString(workspace, "-"))
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func (s *Server) claudeTranscript(id string) (string, error) {
	if !claudeSessionPattern.MatchString(id) {
		return "", ErrNotFound
	}
	for _, dir := range s.claudeProjectDirs() {
		path := filepath.Join(dir, id+".jsonl")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", ErrNotFound
}

func (s *Server) handleClaudeSessions(w http.ResponseWriter, r *http.Request) {
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/claude-sessions"), "/")
	switch {
	case action == "" && r.Method == http.MethodGet:
		s.listClaudeSessions(w)
	case action == "import" && r.Method == http.MethodPost:
		s.importClaudeSession(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) listClaudeSessions(w http.ResponseWriter) {
	owners := map[string]string{}
	if metas, err := s.store.List(); err == nil {
		for _, meta := range metas {
			if meta.ClaudeSessionID != "" {
				owners[meta.ClaudeSessionID] = meta.ID
			}
		}
	}
	sessions := []ClaudeSession{}
	seen := map[string]bool{}
	for _, dir := range s.claudeProjectDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), ".jsonl")
			if !ok || entry.IsDir() || seen[id] || !claudeSessionPattern.MatchString(id) {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.Size() == 0 {
				continue
			}
			seen[id] = true
			title, prompts := summarizeTranscript(filepath.Join(dir, entry.Name()))
			if prompts == 0 && title == "" {
				continue // nothing the user said: a probe or an aborted start
			}
			sessions = append(sessions, ClaudeSession{
				ID: id, Title: title, Updated: info.ModTime().UTC(), Size: info.Size(), Prompts: prompts, WebSession: owners[id],
			})
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// summarizeTranscript reads the head of a transcript for a title (Claude's own
// ai-title, else the first prompt) and how many prompts it holds.
func summarizeTranscript(path string) (string, int) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	// A title the user set (/rename) wins over Claude's generated one.
	custom, title, firstPrompt, prompts := "", "", "", 0
	for i := 0; i < claudeListScanLines; i++ {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry transcriptLine
			if json.Unmarshal(line, &entry) == nil {
				switch {
				case entry.Type == "custom-title" && entry.CustomTitle != "":
					custom = entry.CustomTitle
				case entry.Type == "ai-title" && entry.AITitle != "":
					title = entry.AITitle
				case entry.Type == "summary" && entry.Summary != "" && title == "":
					title = entry.Summary
				case entry.Type == "user" && !entry.IsMeta && !entry.IsSidechain:
					if text := promptText(entry.Message); text != "" {
						prompts++
						if firstPrompt == "" {
							firstPrompt = text
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if custom != "" {
		title = custom
	}
	if title == "" {
		title = firstPrompt
	}
	if title == "" {
		return "", prompts
	}
	return TitleFrom(title), prompts
}

type transcriptLine struct {
	Type        string          `json:"type"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Timestamp   time.Time       `json:"timestamp"`
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	Summary     string          `json:"summary"`
	Message     json.RawMessage `json:"message"`
}

// promptText is what the user typed in a transcript user line: plain text, a
// slash command, or nothing for tool results and Claude Code's own notes.
func promptText(message json.RawMessage) string {
	var envelope struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(message, &envelope) != nil || len(envelope.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(envelope.Content, &text) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(envelope.Content, &parts) != nil {
			return ""
		}
		var texts []string
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		text = strings.Join(texts, "\n")
	}
	text = strings.TrimSpace(text)
	if match := commandName.FindStringSubmatch(text); match != nil {
		name := match[1]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if args := commandArgs.FindStringSubmatch(text); args != nil && strings.TrimSpace(args[1]) != "" {
			name += " " + strings.TrimSpace(args[1])
		}
		return name
	}
	// Command output and caveats Claude Code records as user lines.
	if strings.HasPrefix(text, "<local-command-") || strings.HasPrefix(text, "Caveat:") || strings.HasPrefix(text, "<system-reminder>") {
		return ""
	}
	return text
}

func (s *Server) importClaudeSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.claudeTranscript(body.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Claude Code session not found for this workspace")
		return
	}
	// Importing twice opens the conversation that already resumes it.
	if metas, err := s.store.List(); err == nil {
		for _, meta := range metas {
			if meta.ClaudeSessionID == body.ID {
				writeJSON(w, http.StatusOK, s.view(meta))
				return
			}
		}
	}
	title, _ := summarizeTranscript(path)
	session, err := s.store.Create(Meta{
		Title:           title,
		Provider:        s.defaults.Provider,
		Model:           s.defaults.Model,
		Effort:          s.defaults.Effort,
		Workspace:       s.workspace,
		ClaudeSessionID: body.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	turns, err := importTranscript(path, session)
	if err != nil {
		_ = s.store.Delete(session.ID())
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read transcript: %v", err))
		return
	}
	meta, _ := session.UpdateMeta(func(meta *Meta) { meta.Turns = turns })
	writeJSON(w, http.StatusCreated, s.view(meta))
}

// importTranscript replays a transcript as web events. Sidechain lines belong
// to sub-agents, whose own transcripts are separate, and are skipped.
func importTranscript(path string, session *Session) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 256<<10)
	tools := map[string]string{}
	turns := 0
	open := false // a turn awaiting its closing result
	var last time.Time
	closeTurn := func() {
		if open {
			_, _ = session.AppendAt(EventResult, ResultPayload{}, last)
			open = false
		}
	}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry transcriptLine
			if json.Unmarshal(line, &entry) == nil && !entry.IsSidechain && !entry.IsMeta {
				ts := entry.Timestamp
				if ts.IsZero() {
					ts = last
				}
				switch entry.Type {
				case "user":
					if text := promptText(entry.Message); text != "" {
						closeTurn()
						turns++
						open = true
						_, _ = session.AppendAt(EventUser, UserPayload{Text: text}, ts)
					}
					for _, block := range contentBlocks(entry.Message) {
						if block.Type == "tool_result" {
							_, _ = session.AppendAt(EventToolResult, ToolResultPayload{
								ID: block.ToolUseID, Name: tools[block.ToolUseID], OK: !block.IsError,
								Output: truncateLines(redactSecrets(block.Result), ToolOutputLineLimit),
							}, ts)
						}
					}
				case "assistant":
					open = true
					for _, block := range contentBlocks(entry.Message) {
						switch block.Type {
						case "text":
							if strings.TrimSpace(block.Text) != "" {
								_, _ = session.AppendAt(EventToken, TokenPayload{Text: block.Text + "\n\n"}, ts)
							}
						case "thinking":
							if strings.TrimSpace(block.Thinking) != "" {
								_, _ = session.AppendAt(EventThinking, TokenPayload{Text: block.Thinking}, ts)
							}
						case "tool_use":
							tools[block.ID] = block.Name
							_, _ = session.AppendAt(EventToolUse, ToolUsePayload{
								ID: block.ID, Name: block.Name,
								Summary:     summarizeToolInput(block.Name, block.Input),
								Description: toolDescription(block.Input),
								Input:       toolInput(block.Input),
							}, ts)
						}
					}
				}
				if !ts.IsZero() {
					last = ts
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return turns, readErr
		}
	}
	closeTurn()
	return turns, nil
}
