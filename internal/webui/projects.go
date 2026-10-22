package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// ProjectDefaults are what a new conversation in a project starts with when
// the request names none: provider, model, effort and permission mode.
type ProjectDefaults struct {
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
}

func (d ProjectDefaults) empty() bool { return d == ProjectDefaults{} }

// projectStore keeps every project's defaults in <store>/projects.json,
// keyed by workspace id (derived from the real path, so it survives a
// restart and the folder being added again).
type projectStore struct {
	mu       sync.Mutex
	path     string
	defaults map[string]ProjectDefaults
}

func loadProjects(path string) *projectStore {
	store := &projectStore{path: path, defaults: map[string]ProjectDefaults{}}
	data, err := os.ReadFile(path)
	if err == nil {
		// A damaged file only loses defaults; conversations still start.
		_ = json.Unmarshal(data, &store.defaults)
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ccl web: read %s: %v\n", path, err)
	}
	return store
}

func (p *projectStore) get(id string) ProjectDefaults {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.defaults[id]
}

func (p *projectStore) all() map[string]ProjectDefaults {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]ProjectDefaults, len(p.defaults))
	for id, d := range p.defaults {
		out[id] = d
	}
	return out
}

func (p *projectStore) set(id string, d ProjectDefaults) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.empty() {
		delete(p.defaults, id)
	} else {
		p.defaults[id] = d
	}
	data, err := json.MarshalIndent(p.defaults, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p.path, append(data, '\n'), 0o600)
}

// handleProjectDefaults serves GET and PUT /api/workspaces/<id>/defaults.
func (s *Server) handleProjectDefaults(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.workspaces.byID(id); !ok {
		writeError(w, http.StatusNotFound, ErrWorkspaceNotServed.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.projects.get(id))
	case http.MethodPut:
		var body ProjectDefaults
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		body.Provider = strings.TrimSpace(body.Provider)
		body.Model = strings.TrimSpace(body.Model)
		body.Effort = strings.ToLower(strings.TrimSpace(body.Effort))
		if err := s.validateProvider(body.Provider); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateEffort(body.Effort); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.PermissionMode != "" && !validPermissionMode(body.PermissionMode) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("permission mode: expected one of %s", strings.Join(PermissionModes, ", ")))
			return
		}
		if err := s.projects.set(id, body); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, body)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// applyProjectDefaults fills what a new conversation's request left empty
// from its project's defaults. A provider that no longer exists is skipped
// with its model, so a stale default never blocks a new conversation.
func (s *Server) applyProjectDefaults(meta *Meta, id string, provider, model, effort bool) {
	d := s.projects.get(id)
	if !provider && d.Provider != "" && s.validateProvider(d.Provider) == nil {
		meta.Provider = d.Provider
		if !model {
			meta.Model = d.Model
		}
	} else if !provider && !model && d.Provider == "" && d.Model != "" {
		meta.Model = d.Model
	}
	if !effort && d.Effort != "" {
		meta.Effort = d.Effort
	}
	if d.PermissionMode != "" {
		meta.PermissionMode = d.PermissionMode
	}
}
