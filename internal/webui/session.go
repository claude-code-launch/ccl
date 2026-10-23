// Package webui serves ccl conversations over HTTP and Server-Sent Events.
//
// A conversation is a series of ccl-launched Claude Code processes that share
// one Claude session id and are resumed with --resume. Everything about the
// conversation (its provider, model, effort, history) lives under
// ~/.ccl/serve/sessions; the user's ~/.ccl/config.yaml is only read.
package webui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/google/uuid"
)

// Usage is token, cost and time accounting, for one turn or a whole
// conversation.
type Usage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	DurationMs   int64   `json:"durationMs"`
}

// Meta is the persisted description of one conversation.
type Meta struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	// PermissionMode is the --permission-mode of the next turn; empty uses
	// the server default (bypassPermissions when ccl's bypass mode is on).
	PermissionMode string `json:"permissionMode,omitempty"`
	// ClaudeSessionID is the id Claude Code reported in its first init event.
	// A turn resumes it instead of starting a new conversation.
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`
	Workspace       string `json:"workspace"`
	// Worktree is set when the conversation runs on a branch of its own in
	// a git worktree of Workspace (worktree.go).
	Worktree *SessionWorktree `json:"worktree,omitempty"`
	// Pinned and Archived only change where the page lists the conversation.
	Pinned   bool `json:"pinned,omitempty"`
	Archived bool `json:"archived,omitempty"`
	// ShareToken opens the conversation read-only at /share/<token>.
	ShareToken string    `json:"shareToken,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Turns counts the messages sent, so a conversation list needs no history.
	Turns int `json:"turns"`
	// Usage is the conversation total: the sum of every turn's TurnUsage.
	Usage Usage `json:"usage"`
	// ClaudeCostUSD is the running cost Claude Code last reported for the
	// session. The next turn's cost is the difference from it.
	ClaudeCostUSD float64 `json:"claudeCostUsd,omitempty"`
}

// recordTurn adds one turn's result to the conversation totals and returns
// what the turn alone spent.
func (m *Meta) recordTurn(reported Usage) Usage {
	turn := reported
	switch {
	case reported.CostUSD >= m.ClaudeCostUSD:
		turn.CostUSD = reported.CostUSD - m.ClaudeCostUSD
	case reported.CostUSD == 0:
		// A crash or startup-error result carries zeroed values; the session's
		// running total has not really gone back to zero.
	default:
		// The running total restarted (a new or cleared Claude session).
	}
	if reported.CostUSD > 0 {
		m.ClaudeCostUSD = reported.CostUSD
	}
	m.Usage.InputTokens += turn.InputTokens
	m.Usage.OutputTokens += turn.OutputTokens
	m.Usage.CostUSD += turn.CostUSD
	m.Usage.DurationMs += turn.DurationMs
	return turn
}

// Event is one persisted entry of a conversation's history. Seq is strictly
// increasing within a conversation, so a client reconnecting with ?since= can
// ask for exactly what it missed.
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	TS      int64           `json:"ts"`
}

const (
	// TitleLimit is the rune count of the title derived from the first message.
	TitleLimit = 40
	// maxEventLine bounds one events.jsonl line so a corrupt write cannot make
	// the reader allocate unbounded memory.
	maxEventLine = 8 << 20
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ErrNotFound reports a conversation that does not exist on disk.
var ErrNotFound = errors.New("session not found")

// Store is the on-disk conversation repository:
//
//	<root>/<id>/meta.json
//	<root>/<id>/events.jsonl
type Store struct {
	root string

	// OnAppend is called after a session stores an event. The server uses it to
	// wake open SSE streams.
	OnAppend func(sessionID string)

	mu   sync.Mutex
	open map[string]*Session
}

// NewStore creates root when needed and returns a store over it.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create web session directory: %w", err)
	}
	return &Store{root: root, open: make(map[string]*Session)}, nil
}

// DefaultRoot is where ccl serve keeps conversations.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "serve", "sessions"), nil
}

// Root is the directory holding every conversation.
func (s *Store) Root() string { return s.root }

// List returns every conversation, most recently updated first.
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read web sessions: %w", err)
	}
	metas := make([]Meta, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !sessionIDPattern.MatchString(entry.Name()) {
			continue
		}
		meta, err := readMeta(filepath.Join(s.root, entry.Name()))
		if err != nil {
			// A half-written directory must not hide the other conversations.
			continue
		}
		if meta.Turns == 0 {
			// Opening counts the turns of an older conversation (see openSession).
			if session, err := s.Open(meta.ID); err == nil {
				meta = session.Meta()
			}
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].ID < metas[j].ID
		}
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})
	return metas, nil
}

// Create writes a new conversation and returns it.
func (s *Store) Create(meta Meta) (*Session, error) {
	if meta.ID == "" {
		meta.ID = uuid.NewString()
	}
	if !sessionIDPattern.MatchString(meta.ID) {
		return nil, fmt.Errorf("invalid session id %q", meta.ID)
	}
	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	meta.Title = strings.TrimSpace(meta.Title)

	dir := filepath.Join(s.root, meta.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	if err := writeMeta(dir, meta); err != nil {
		return nil, err
	}
	session, err := s.openSession(dir, meta)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open[meta.ID] = session
	s.mu.Unlock()
	return session, nil
}

// Open returns the conversation with that id.
func (s *Store) Open(id string) (*Session, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	if session, ok := s.open[id]; ok {
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()

	dir := filepath.Join(s.root, id)
	meta, err := readMeta(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session, err := s.openSession(dir, meta)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open[id] = session
	s.mu.Unlock()
	return session, nil
}

func (s *Store) openSession(dir string, meta Meta) (*Session, error) {
	session := &Session{dir: dir, meta: meta, store: s}
	events, err := readEvents(dir, 0)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		session.seq = events[len(events)-1].Seq
	}
	// A conversation from before the turn counter is counted once, from the
	// history this read already loaded, and keeps the count.
	if meta.Turns == 0 {
		if turns := countTurns(events); turns > 0 {
			session.meta.Turns = turns
			_ = writeMeta(dir, session.meta)
		}
	}
	return session, nil
}

func countTurns(events []Event) int {
	turns := 0
	for _, event := range events {
		if event.Type == EventUser {
			turns++
		}
	}
	return turns
}

// Delete removes a conversation and its history.
func (s *Store) Delete(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return ErrNotFound
	}
	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	s.mu.Lock()
	delete(s.open, id)
	s.mu.Unlock()
	return nil
}

// Session is one open conversation: its metadata and append-only history.
type Session struct {
	dir   string
	store *Store

	mu   sync.Mutex
	meta Meta
	seq  int64
}

// ID is the conversation id.
func (s *Session) ID() string { return s.meta.ID }

// Meta returns a copy of the conversation metadata.
func (s *Session) Meta() Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// UpdateMeta applies fn to the metadata and persists it.
func (s *Session) UpdateMeta(fn func(*Meta)) (Meta, error) {
	return s.updateMeta(fn, true)
}

// RelabelMeta changes how a conversation is listed (pinned, archived)
// without making it the most recently updated.
func (s *Session) RelabelMeta(fn func(*Meta)) (Meta, error) {
	return s.updateMeta(fn, false)
}

func (s *Session) updateMeta(fn func(*Meta), touch bool) (Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.meta)
	if touch {
		s.meta.UpdatedAt = time.Now().UTC()
	}
	if err := writeMeta(s.dir, s.meta); err != nil {
		return Meta{}, err
	}
	return s.meta, nil
}

// Append stores one event and returns it with its assigned seq.
func (s *Session) Append(kind string, payload any) (Event, error) {
	return s.AppendAt(kind, payload, time.Time{})
}

// AppendAt stores an event that happened at ts (zero means now), such as one
// replayed from an imported transcript.
func (s *Session) AppendAt(kind string, payload any, ts time.Time) (Event, error) {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("encode %s event: %w", kind, err)
		}
		raw = data
	}
	return s.appendRaw(kind, raw, ts)
}

func (s *Session) appendRaw(kind string, payload json.RawMessage, ts time.Time) (Event, error) {
	event, err := s.write(kind, payload, ts)
	if err == nil && s.store != nil && s.store.OnAppend != nil {
		s.store.OnAppend(s.ID())
	}
	return event, err
}

func (s *Session) write(kind string, payload json.RawMessage, ts time.Time) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts.IsZero() {
		ts = time.Now()
	}
	s.seq++
	event := Event{Seq: s.seq, Type: kind, Payload: payload, TS: ts.UnixMilli()}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event: %w", kind, err)
	}
	path := filepath.Join(s.dir, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Event{}, fmt.Errorf("open session history: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("append session history: %w", err)
	}
	// Metadata is written on turn boundaries (see Touch), not per token: a
	// streamed answer appends thousands of events.
	return event, nil
}

// Events returns every stored event with Seq greater than since.
func (s *Session) Events(since int64) ([]Event, error) {
	return readEvents(s.dir, since)
}

func readEvents(dir string, since int64) ([]Event, error) {
	path := filepath.Join(dir, "events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session history: %w", err)
	}
	defer file.Close()

	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue // A torn final line is a crash, not a reason to lose history.
		}
		if event.Seq <= since {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	return events, nil
}

// LastSeq is the newest event seq, 0 for an empty conversation.
func (s *Session) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Touch persists the metadata as it stands, refreshing UpdatedAt. It is called
// once per turn rather than per event.
func (s *Session) Touch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.UpdatedAt = time.Now().UTC()
	return writeMeta(s.dir, s.meta)
}

func readMeta(dir string) (Meta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("decode session metadata: %w", err)
	}
	if meta.ID == "" {
		meta.ID = filepath.Base(dir)
	}
	return meta, nil
}

func writeMeta(dir string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "meta.json"), data, 0o600); err != nil {
		return fmt.Errorf("write session metadata: %w", err)
	}
	return nil
}

// TitleFrom derives a conversation title from its first user message.
func TitleFrom(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " "))
	if text == "" {
		return "New conversation"
	}
	if utf8.RuneCountInString(text) <= TitleLimit {
		return text
	}
	runes := []rune(text)
	return string(runes[:TitleLimit]) + "…"
}
