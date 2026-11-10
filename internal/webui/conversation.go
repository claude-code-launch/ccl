package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

type summary struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Title     string    `json:"title"`
	Updated   time.Time `json:"updated"`
}

type record struct {
	summary
	Provider string        `json:"provider"`
	AgentID  acp.SessionId `json:"agent_id,omitempty"`
}

type event struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`
	Text     string `json:"text,omitempty"`
	Status   string `json:"status,omitempty"`
	Detail   string `json:"detail,omitempty"`
	CanAllow bool   `json:"can_allow,omitempty"`
	Append   bool   `json:"append,omitempty"`
}

type approval struct {
	canAllow bool
	decision chan string
}

type operation struct {
	ctx     context.Context
	cancel  context.CancelFunc
	agentID acp.SessionId
	replay  []event // Staged until session/load returns successfully.
	live    bool
}

type conversation struct {
	mu      sync.Mutex
	record  record
	active  *operation
	loaded  bool
	pending map[string]approval
	events  []event
	changed chan struct{}
}

func newConversation(rec record) *conversation {
	return &conversation{
		record: rec, loaded: rec.AgentID == "", pending: make(map[string]approval),
		events: []event{{Kind: "state", Text: "idle"}}, changed: make(chan struct{}),
	}
}

func (c *conversation) emitLocked(events ...event) {
	c.events = append(c.events, events...)
	close(c.changed)
	c.changed = make(chan struct{})
}

func (s *server) recordDir(id string) string {
	return filepath.Join(s.opts.StateDir, "sessions", id)
}

func (s *server) save(rec record) error {
	dir := s.recordDir(rec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, "session.json"), raw, 0o600)
}

func (s *server) loadRecords() error {
	entries, err := os.ReadDir(filepath.Join(s.opts.StateDir, "sessions"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || uuid.Validate(entry.Name()) != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.recordDir(entry.Name()), "session.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var rec record
		if err := json.Unmarshal(raw, &rec); err != nil || rec.ID != entry.Name() {
			oauthproxy.LogWarnf("skipping invalid browser session record %s", entry.Name())
			continue
		}
		if rec.Provider == s.opts.Provider.Name && slices.Contains(s.opts.Workspaces, rec.Workspace) {
			s.chats[rec.ID] = newConversation(rec)
		}
	}
	return nil
}

// startOperation starts a prompt, or hydrates saved history for a stream.
// An operation owns the chat and its ACP process until cleanup is complete.
func (s *server) startOperation(w http.ResponseWriter, chat *conversation, prompt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if s.closing {
		http.Error(w, "Server is stopping", http.StatusServiceUnavailable)
		return false
	}
	if prompt == "" && (chat.loaded || chat.active != nil) {
		return true
	}
	if chat.active != nil {
		http.Error(w, "This session already has work in progress", http.StatusConflict)
		return false
	}
	select {
	case s.slots <- struct{}{}:
	default:
		http.Error(w, "All Grok workers are busy. Try again shortly.", http.StatusServiceUnavailable)
		return false
	}
	ctx, cancel := context.WithCancel(s.ctx)
	rec := chat.record
	op := &operation{ctx: ctx, cancel: cancel, agentID: rec.AgentID}
	chat.active = op
	state := "working"
	if prompt == "" {
		state = "loading"
	}
	chat.emitLocked(event{Kind: "state", Text: state})
	s.workers.Go(func() {
		outcome, err := s.run(chat, op, rec, prompt)
		<-s.slots
		chat.finish(op, outcome, err)
	})
	if prompt != "" {
		w.WriteHeader(http.StatusAccepted)
	}
	return true
}

func (c *conversation) finish(op *operation, outcome string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cancelled := op.ctx.Err() != nil
	op.cancel()
	c.active = nil
	for id := range c.pending {
		delete(c.pending, id)
		c.emitLocked(event{Kind: "permission_closed", ID: id})
	}
	switch {
	case cancelled:
		c.emitLocked(event{Kind: "notice", Text: "Stopped."})
	case err != nil:
		c.emitLocked(event{Kind: "notice", Text: err.Error(), Status: "error"})
	case outcome != "" && outcome != string(acp.StopReasonEndTurn):
		c.emitLocked(event{Kind: "notice", Text: fmt.Sprintf("Grok ended the turn (%s).", outcome)})
	}
	c.emitLocked(event{Kind: "state", Text: "idle"})
}

func (s *server) beginPrompt(c *conversation, op *operation, rec record, prompt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := op.ctx.Err(); err != nil {
		return err
	}
	if prompt != "" {
		if rec.AgentID == "" {
			firstLine, _, _ := strings.Cut(prompt, "\n")
			chars := []rune(strings.TrimSpace(firstLine))
			if len(chars) > 60 {
				rec.Title = string(chars[:60]) + "…"
			} else {
				rec.Title = string(chars)
			}
		}
		rec.AgentID, rec.Updated = op.agentID, time.Now()
		if err := s.save(rec); err != nil {
			return workerError("Could not save the Grok session.", err)
		}
		c.record = rec
	}
	c.loaded = true
	c.emitLocked(op.replay...)
	op.replay = nil
	if prompt != "" {
		op.live = true
		c.emitLocked(event{Kind: "title", Text: rec.Title}, event{Kind: "user", Text: prompt})
	}
	return nil
}
