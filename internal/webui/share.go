package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A conversation can be shared read-only: POST /api/sessions/:id/share gives
// it a random token, and /share/<token> (the page) reads /api/share/<token>
// without the server's own token. The link shows the whole conversation as
// stored, tool output included, and the worktree diff; DELETE revokes it.
// Who can open it is up to where ccl web listens: a loopback server is only
// reachable from this machine.

var shareTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxWorktreeDiff bounds the diff a worktree reports for an export or a
// shared page.
const maxWorktreeDiff = 2 << 20

// SharedSession is what a share link reveals.
type SharedSession struct {
	Title     string           `json:"title"`
	Provider  string           `json:"provider"`
	Model     string           `json:"model"`
	Workspace string           `json:"workspace"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
	Worktree  *SessionWorktree `json:"worktree,omitempty"`
	Events    []Event          `json:"events"`
	Diff      *WorktreeDiff    `json:"diff,omitempty"`
}

// WorktreeDiff is a worktree's change against its base, uncommitted tracked
// changes included.
type WorktreeDiff struct {
	Base      string `json:"base"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated,omitempty"`
}

func newShareToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// handleShare serves POST (create or return the link) and DELETE (revoke) on
// /api/sessions/:id/share.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request, session *Session) {
	switch r.Method {
	case http.MethodPost:
		meta := session.Meta()
		if meta.ShareToken == "" {
			token, err := newShareToken()
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if meta, err = session.RelabelMeta(func(meta *Meta) { meta.ShareToken = token }); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": meta.ShareToken, "path": "/share/" + meta.ShareToken})
	case http.MethodDelete:
		if _, err := session.RelabelMeta(func(meta *Meta) { meta.ShareToken = "" }); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// sharedSession finds the conversation a share token names.
func (s *Server) sharedSession(token string) (*Session, bool) {
	if !shareTokenPattern.MatchString(token) {
		return nil, false
	}
	metas, err := s.store.List()
	if err != nil {
		return nil, false
	}
	for _, meta := range metas {
		if meta.ShareToken != "" && subtle.ConstantTimeCompare([]byte(meta.ShareToken), []byte(token)) == 1 {
			session, err := s.store.Open(meta.ID)
			return session, err == nil
		}
	}
	return nil, false
}

// handleShared serves the public /api/share/<token>[/uploads/<file>].
func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The token stays out of other sites' logs.
	w.Header().Set("Referrer-Policy", "no-referrer")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/share/"), "/"), "/")
	session, ok := s.sharedSession(parts[0])
	if !ok {
		writeError(w, http.StatusNotFound, "this link is not shared")
		return
	}
	switch {
	case len(parts) == 3 && parts[1] == "uploads":
		s.serveUpload(w, session, parts[2])
	case len(parts) == 1:
		meta := session.Meta()
		events, err := session.Events(0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if events == nil {
			events = []Event{}
		}
		shared := SharedSession{
			Title: meta.Title, Provider: meta.Provider, Model: meta.Model, Workspace: baseName(meta.Workspace),
			CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, Worktree: meta.Worktree, Events: events,
		}
		if diff, err := s.sessionDiff(r.Context(), meta); err == nil {
			shared.Diff = diff
		}
		writeJSON(w, http.StatusOK, shared)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// baseName shows a shared page the project's folder name, not where it is
// on this machine.
func baseName(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// sessionDiff is a worktree conversation's diff against its base.
func (s *Server) sessionDiff(ctx context.Context, meta Meta) (*WorktreeDiff, error) {
	if meta.Worktree == nil {
		return nil, ErrWorktreeMissing
	}
	ws, ok := s.workspaces.byPath(meta.Workspace)
	if !ok {
		return nil, ErrWorkspaceNotServed
	}
	tree, err := s.worktreeWorkspace(ws, meta.Worktree.Name)
	if err != nil {
		return nil, err
	}
	base := meta.Worktree.Base
	if base == "" {
		base = meta.Worktree.BaseCommit
	}
	// The merge base, so commits the base gained since do not show as
	// reverted here.
	from, err := s.gitText(ctx, tree, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	result, err := s.git(ctx, tree, nil, "diff", "--no-color", "--no-ext-diff", from, "--")
	if err != nil {
		return nil, err
	}
	diff := &WorktreeDiff{Base: base, Diff: string(result.stdout)}
	if len(diff.Diff) > maxWorktreeDiff {
		diff.Diff, diff.Truncated = diff.Diff[:maxWorktreeDiff], true
	}
	return diff, nil
}
