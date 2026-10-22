package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Workspaces are the directories conversations may run in. They are fixed when
// ccl web starts — each --workspace, and each directory directly inside a
// --workspace-root — and the page only ever chooses among them: it never names
// a path of its own.

// Workspace is one directory the server offers.
type Workspace struct {
	// ID is derived from the real path, so it is stable across restarts.
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// real is Path with symbolic links resolved; file paths are checked
	// against it.
	real string
}

// ErrWorkspaceNotServed reports a conversation or request for a directory
// this server does not offer (any more).
var ErrWorkspaceNotServed = errors.New("this workspace is not served by ccl web; start it with --workspace or --workspace-root covering it")

// rootRescan bounds how often --workspace-root directories are listed again,
// so a repository cloned meanwhile appears without a restart.
const rootRescan = 5 * time.Second

type workspaceSet struct {
	fixed []Workspace
	roots []string

	mu       sync.Mutex
	cached   []Workspace
	cachedAt time.Time
}

func newWorkspace(path string) (Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("workspace %s is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	sum := sha256.Sum256([]byte(real))
	return Workspace{ID: hex.EncodeToString(sum[:6]), Name: filepath.Base(abs), Path: abs, real: real}, nil
}

func newWorkspaceSet(paths, roots []string) (*workspaceSet, error) {
	set := &workspaceSet{}
	seen := map[string]bool{}
	for _, path := range paths {
		workspace, err := newWorkspace(path)
		if err != nil {
			return nil, err
		}
		if !seen[workspace.real] {
			seen[workspace.real] = true
			set.fixed = append(set.fixed, workspace)
		}
	}
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace root: %w", err)
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("workspace root %s is not a directory", abs)
		}
		set.roots = append(set.roots, abs)
	}
	if len(set.fixed) == 0 && len(set.roots) == 0 {
		return nil, errors.New("no workspace: pass --workspace or --workspace-root")
	}
	return set, nil
}

// list is every workspace: the fixed ones first, then each root's
// subdirectories by name. Hidden directories are not workspaces.
func (w *workspaceSet) list() []Workspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cached != nil && time.Since(w.cachedAt) < rootRescan {
		return w.cached
	}
	out := append([]Workspace(nil), w.fixed...)
	seen := map[string]bool{}
	for _, workspace := range out {
		seen[workspace.real] = true
	}
	for _, root := range w.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		var found []Workspace
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			workspace, err := newWorkspace(filepath.Join(root, entry.Name()))
			if err != nil || seen[workspace.real] {
				continue
			}
			seen[workspace.real] = true
			found = append(found, workspace)
		}
		sort.Slice(found, func(i, j int) bool { return strings.ToLower(found[i].Name) < strings.ToLower(found[j].Name) })
		out = append(out, found...)
	}
	w.cached, w.cachedAt = out, time.Now()
	return out
}

func (w *workspaceSet) byID(id string) (Workspace, bool) {
	for _, workspace := range w.list() {
		if workspace.ID == id {
			return workspace, true
		}
	}
	return Workspace{}, false
}

// byPath finds the workspace a conversation recorded, by its path as given or
// with links resolved.
func (w *workspaceSet) byPath(path string) (Workspace, bool) {
	if path == "" {
		return Workspace{}, false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	for _, workspace := range w.list() {
		if workspace.Path == path || workspace.real == real {
			return workspace, true
		}
	}
	return Workspace{}, false
}

// fallback is the workspace a request without one means.
func (w *workspaceSet) fallback() (Workspace, bool) {
	list := w.list()
	if len(list) == 0 {
		return Workspace{}, false
	}
	return list[0], true
}

// workspaceFor is the workspace a request names with ?workspace=<id>, or the
// first one when it names none.
func (s *Server) workspaceFor(r *http.Request) (Workspace, error) {
	if id := r.URL.Query().Get("workspace"); id != "" {
		if workspace, ok := s.workspaces.byID(id); ok {
			return workspace, nil
		}
		return Workspace{}, ErrWorkspaceNotServed
	}
	if workspace, ok := s.workspaces.fallback(); ok {
		return workspace, nil
	}
	return Workspace{}, ErrWorkspaceNotServed
}

// withWorkspace hands a handler the request's workspace, or refuses it.
func (s *Server) withWorkspace(next func(http.ResponseWriter, *http.Request, Workspace)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		workspace, err := s.workspaceFor(r)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		next(w, r, workspace)
	}
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": s.workspaces.list()})
}

// WorkspaceCount is how many workspaces the server offers right now.
func (s *Server) WorkspaceCount() int { return len(s.workspaces.list()) }
