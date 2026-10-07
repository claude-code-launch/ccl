package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

type persistedSession struct {
	SessionID       string `json:"sessionId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	CWD             string `json:"cwd"`
}

type sessionStore struct {
	dir string
}

// DefaultSessionStorePath returns the per-user directory used for ACP session
// mappings that must survive an agent process restart.
func DefaultSessionStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "acp", "sessions"), nil
}

func newSessionStore(dir string) *sessionStore {
	if dir == "" {
		return nil
	}
	return &sessionStore{dir: dir}
}

func (s *sessionStore) save(entry persistedSession) error {
	if s == nil || entry.SessionID == "" || entry.ClaudeSessionID == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create ACP session store: %w", err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("secure ACP session store: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal ACP session: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path(entry.SessionID), data, 0o600); err != nil {
		return fmt.Errorf("store ACP session: %w", err)
	}
	return nil
}

// sessionStoreMaxAge matches Claude Code's default transcript retention: a
// mapping older than that points at a transcript that is likely gone.
const sessionStoreMaxAge = 30 * 24 * time.Hour

// prune removes mappings not written for maxAge, plus temp files a crashed
// save left behind. Best effort.
func (s *sessionStore) prune(maxAge time.Duration, now time.Time) {
	if s == nil {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		// Mappings, plus temporary files a crashed save left behind (current
		// ".<id>.json.tmp-*" and the older ".session-*" names).
		leftover := strings.HasPrefix(name, ".") && (strings.Contains(name, ".tmp-") || strings.HasPrefix(name, ".session-"))
		if entry.IsDir() || !(strings.HasSuffix(name, ".json") || leftover) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, name))
	}
}

func (s *sessionStore) load(id string) (persistedSession, error) {
	var entry persistedSession
	if s == nil {
		return entry, os.ErrNotExist
	}
	path, err := s.validPath(id)
	if err != nil {
		return entry, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return entry, err
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, fmt.Errorf("decode ACP session: %w", err)
	}
	if entry.SessionID != id || entry.ClaudeSessionID == "" || entry.CWD == "" {
		return entry, fmt.Errorf("invalid ACP session mapping")
	}
	if _, err := resolveSessionCWD(entry.CWD); err != nil {
		return entry, fmt.Errorf("invalid ACP session cwd: %w", err)
	}
	return entry, nil
}

func (s *sessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *sessionStore) validPath(id string) (string, error) {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return "", os.ErrNotExist
	}
	return s.path(id), nil
}
