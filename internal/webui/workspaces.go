package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// Workspaces are the directories conversations may run in: each --workspace,
// each directory directly inside a --workspace-root, and the directories the
// page added ("open another folder"), which are kept in <store>/workspaces.json.
// Requests name a workspace by id, never by path; only adding one takes a path,
// and --lock-workspaces turns that off.

// Workspace is one directory the server offers.
type Workspace struct {
	// ID is derived from the real path, so it is stable across restarts.
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// Added marks a directory the page added; only those can be removed.
	Added bool `json:"added,omitempty"`
	// real is Path with symbolic links resolved; file paths are checked
	// against it.
	real string
}

// ErrWorkspaceNotServed reports a conversation or request for a directory
// this server does not offer (any more).
var ErrWorkspaceNotServed = errors.New("this workspace is not served by ccl web; add its folder again or start ccl web with --workspace covering it")

// ErrWorkspacesLocked refuses adding a folder under --lock-workspaces.
var ErrWorkspacesLocked = errors.New("ccl web was started with --lock-workspaces; folders cannot be added from the page")

// rootRescan bounds how often --workspace-root directories are listed again,
// so a repository cloned meanwhile appears without a restart.
const rootRescan = 5 * time.Second

type workspaceSet struct {
	fixed []Workspace
	roots []string
	// added are the page's folders, saved at addedPath; locked refuses more.
	added     []Workspace
	addedPath string
	locked    bool

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
	// Roots may be empty here: a server can start from added folders alone.
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

// loadAdded reads the page's folders; one that no longer exists is skipped
// but kept in the file, so a remounted disk brings it back.
func (w *workspaceSet) loadAdded(path string, locked bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.addedPath, w.locked = path, locked
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var paths []string
	if json.Unmarshal(data, &paths) != nil {
		return
	}
	for _, path := range paths {
		if workspace, err := newWorkspace(path); err == nil {
			workspace.Added = true
			w.added = append(w.added, workspace)
		}
	}
	w.cached = nil
}

func (w *workspaceSet) saveAddedLocked() error {
	paths := make([]string, 0, len(w.added))
	for _, workspace := range w.added {
		paths = append(paths, workspace.Path)
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(w.addedPath, data, 0o600)
}

// add makes a directory a workspace. A directory already offered is returned
// as it is.
func (w *workspaceSet) add(path string) (Workspace, error) {
	if w.isLocked() {
		return Workspace{}, ErrWorkspacesLocked
	}
	workspace, err := newWorkspace(path)
	if err != nil {
		return Workspace{}, err
	}
	for _, existing := range w.list() {
		if existing.real == workspace.real {
			return existing, nil
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	workspace.Added = true
	w.added = append(w.added, workspace)
	w.cached = nil
	if err := w.saveAddedLocked(); err != nil {
		w.added = w.added[:len(w.added)-1]
		return Workspace{}, err
	}
	return workspace, nil
}

// remove forgets an added folder; the directory itself is untouched.
func (w *workspaceSet) remove(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, workspace := range w.added {
		if workspace.ID == id {
			w.added = append(w.added[:i:i], w.added[i+1:]...)
			w.cached = nil
			_ = w.saveAddedLocked()
			return true
		}
	}
	return false
}

func (w *workspaceSet) isLocked() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.locked
}

// list is every workspace: the fixed ones first, then the added ones, then each
// root's subdirectories by name. Hidden directories are not workspaces.
func (w *workspaceSet) list() []Workspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cached != nil && time.Since(w.cachedAt) < rootRescan {
		return w.cached
	}
	out := []Workspace{}
	seen := map[string]bool{}
	for _, workspace := range append(append([]Workspace(nil), w.fixed...), w.added...) {
		if !seen[workspace.real] {
			seen[workspace.real] = true
			out = append(out, workspace)
		}
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
// first one when it names none; &worktree=<name> narrows it to a
// conversation's worktree.
func (s *Server) workspaceFor(r *http.Request) (Workspace, error) {
	workspace, ok := s.workspaces.fallback()
	if id := r.URL.Query().Get("workspace"); id != "" {
		workspace, ok = s.workspaces.byID(id)
	}
	if !ok {
		return Workspace{}, ErrWorkspaceNotServed
	}
	if name := r.URL.Query().Get("worktree"); name != "" {
		return s.worktreeWorkspace(workspace, name)
	}
	return workspace, nil
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

// handleWorkspaces serves GET (the list) and POST {path} (add a folder) on
// /api/workspaces, and DELETE /api/workspaces/<id> (forget an added folder).
func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workspaces"), "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"workspaces": s.workspaces.list(), "canAdd": !s.workspaces.isLocked(), "defaults": s.projects.all()})
	case strings.HasSuffix(id, "/defaults"):
		s.handleProjectDefaults(w, r, strings.TrimSuffix(id, "/defaults"))
	case id == "" && r.Method == http.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		path, err := expandHome(strings.TrimSpace(body.Path))
		if err != nil || !filepath.IsAbs(path) {
			writeError(w, http.StatusBadRequest, "path must be absolute")
			return
		}
		workspace, err := s.workspaces.add(path)
		switch {
		case errors.Is(err, ErrWorkspacesLocked):
			writeError(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSON(w, http.StatusCreated, workspace)
		}
	case id != "" && r.Method == http.MethodDelete:
		if !s.workspaces.remove(id) {
			writeError(w, http.StatusNotFound, "only a folder added from the page can be removed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// DirEntry is one folder in the folder picker.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Repo marks a git work tree.
	Repo bool `json:"repo,omitempty"`
}

// maxDirEntries bounds one folder listing.
const maxDirEntries = 2000

// handleDirectories lists the folders inside ?path= (default: the home
// directory) for the folder picker, and POST {path, name} creates one folder
// there ("new project"). Files are never listed. It is off under
// --lock-workspaces, where nothing could be added anyway.
func (s *Server) handleDirectories(w http.ResponseWriter, r *http.Request) {
	if s.workspaces.isLocked() {
		writeError(w, http.StatusForbidden, ErrWorkspacesLocked.Error())
		return
	}
	if r.Method == http.MethodPost {
		s.createDirectory(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	home, _ := os.UserHomeDir()
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = home
	}
	path, err := expandHome(path)
	if err != nil || !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	path = filepath.Clean(path)
	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	hidden := r.URL.Query().Get("hidden") == "1"
	dirs := []DirEntry{}
	for _, entry := range entries {
		if len(dirs) >= maxDirEntries {
			break
		}
		if !hidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		full := filepath.Join(path, entry.Name())
		// A link to a folder is offered like a folder.
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(full, ".git"))
		dirs = append(dirs, DirEntry{Name: entry.Name(), Path: full, Repo: gitErr == nil})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	_, gitErr := os.Stat(filepath.Join(path, ".git"))
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "parent": parent, "home": home, "repo": gitErr == nil, "entries": dirs})
}

// createDirectory makes one new folder inside an existing one. The name is a
// single path segment; an existing folder is an error, never reused.
func (s *Server) createDirectory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parent, err := expandHome(strings.TrimSpace(body.Path))
	if err != nil || !filepath.IsAbs(parent) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		writeError(w, http.StatusBadRequest, "name must be one folder name")
		return
	}
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, "parent folder not found")
		return
	}
	full := filepath.Join(filepath.Clean(parent), name)
	if err := os.Mkdir(full, 0o755); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrExist) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": full})
}

// expandHome turns a leading ~ into the home directory.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

// WorkspaceCount is how many workspaces the server offers right now.
func (s *Server) WorkspaceCount() int { return len(s.workspaces.list()) }
