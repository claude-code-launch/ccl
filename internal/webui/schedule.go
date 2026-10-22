package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/google/uuid"
)

// Scheduled messages: a message the user wants sent later. They survive a
// restart of ccl web; one whose time passed while it was down goes out when
// it is back. A conversation busy at the time gets it after its turn.

// Schedule is one message waiting for its time.
type Schedule struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

const (
	scheduleTick    = 10 * time.Second
	maxScheduleSpan = 366 * 24 * time.Hour
)

type scheduler struct {
	mu    sync.Mutex
	path  string
	items []Schedule
}

func newScheduler(path string) *scheduler {
	sched := &scheduler{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &sched.items)
	}
	return sched
}

func (s *scheduler) saveLocked() error {
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, data, 0o600)
}

func (s *scheduler) forSession(id string) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Schedule{}
	for _, item := range s.items {
		if item.Session == id {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func (s *scheduler) add(item Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	return s.saveLocked()
}

func (s *scheduler) remove(session, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.items {
		if item.ID == id && (session == "" || item.Session == session) {
			s.items = append(s.items[:i], s.items[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

func (s *scheduler) due(now time.Time) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Schedule
	for _, item := range s.items {
		if !item.At.After(now) {
			due = append(due, item)
		}
	}
	return due
}

// runSchedules sends due messages; it runs for the life of the server.
func (s *Server) runSchedules() {
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for range ticker.C {
		s.sendDue(time.Now())
	}
}

func (s *Server) sendDue(now time.Time) {
	if s.closing.Load() {
		return
	}
	for _, item := range s.schedules.due(now) {
		session, err := s.store.Open(item.Session)
		if errors.Is(err, ErrNotFound) {
			s.schedules.remove("", item.ID)
			continue
		}
		if err != nil || s.timeline.Running(item.Session) {
			continue // try again on the next tick
		}
		if !s.schedules.remove("", item.ID) {
			continue // cancelled meanwhile
		}
		_, _ = s.startTurn(session, item.Text, nil)
	}
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	switch {
	case id == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"schedules": s.schedules.forSession(session.ID())})
	case id == "" && r.Method == http.MethodPost:
		var body struct {
			At   time.Time `json:"at"`
			Text string    `json:"text"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		now := time.Now()
		if !body.At.After(now) || body.At.After(now.Add(maxScheduleSpan)) {
			writeError(w, http.StatusBadRequest, "the time must be in the future, within a year")
			return
		}
		item := Schedule{ID: uuid.NewString(), Session: session.ID(), At: body.At.UTC(), Text: text}
		if err := s.schedules.add(item); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case id != "" && r.Method == http.MethodDelete:
		if !s.schedules.remove(session.ID(), id) {
			writeError(w, http.StatusNotFound, "no such scheduled message")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func schedulePath(storeRoot string) string {
	return filepath.Join(storeRoot, "schedules.json")
}
