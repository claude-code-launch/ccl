package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A conversation may run in a git worktree of its own: a new branch checked
// out under the store (never inside the project, so the project's git status
// stays clean). Its files, git, terminal and MCP requests name it with
// ?workspace=<project id>&worktree=<name>; merging into the branch it started
// from and pushing are explicit actions on the conversation.

// SessionWorktree is the worktree a conversation runs in.
type SessionWorktree struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// Base is the branch the project had checked out when the worktree was
	// made; empty when its HEAD was detached. BaseCommit is where it started.
	Base       string `json:"base,omitempty"`
	BaseCommit string `json:"baseCommit"`
}

// WorktreeStatus is a session worktree as the page shows it.
type WorktreeStatus struct {
	SessionWorktree
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Ahead and Behind count commits against the base (branch, else commit).
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Dirty counts uncommitted changes in the worktree.
	Dirty int `json:"dirty"`
	// Files are the paths the branch changed since the base.
	Files []WorktreeFile `json:"files"`
	// Current is the branch the project itself has checked out: a merge
	// goes there, so it must be Base.
	Current  string `json:"current,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Upstream string `json:"upstream,omitempty"`
}

type WorktreeFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

var (
	// ErrWorktreeMissing refuses a turn of a conversation whose worktree was
	// removed outside ccl web.
	ErrWorktreeMissing = errors.New("this conversation's git worktree no longer exists")
	errNotARepository  = errors.New("the project is not a git repository")
	errNoCommits       = errors.New("the repository has no commits yet")
)

var worktreeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// maxWorktreeFiles bounds the changed-file list of a branch.
const maxWorktreeFiles = 500

// worktreeLocalFiles are untracked files a worktree would miss: the project's
// personal Claude Code instructions and settings.
var worktreeLocalFiles = []string{"CLAUDE.local.md", filepath.Join(".claude", "settings.local.json")}

func (s *Server) worktreeDir(ws Workspace, name string) string {
	return filepath.Join(s.store.Root(), ".worktrees", ws.ID, name)
}

// worktreeWorkspace narrows a project to one of its session worktrees.
func (s *Server) worktreeWorkspace(ws Workspace, name string) (Workspace, error) {
	if !worktreeNamePattern.MatchString(name) {
		return Workspace{}, ErrWorktreeMissing
	}
	dir := s.worktreeDir(ws, name)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Workspace{}, ErrWorktreeMissing
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return Workspace{}, ErrWorktreeMissing
	}
	return Workspace{ID: ws.ID, Name: ws.Name, Path: dir, real: real}, nil
}

// gitText runs git and returns its trimmed output, or an error carrying
// what git said.
func (s *Server) gitText(ctx context.Context, ws Workspace, args ...string) (string, error) {
	result, err := s.git(ctx, ws, nil, args...)
	if err != nil {
		return "", err
	}
	if result.code != 0 {
		message := result.stderr
		if message == "" {
			message = strings.TrimSpace(string(result.stdout))
		}
		return "", fmt.Errorf("git %s: %s", args[0], message)
	}
	return strings.TrimSpace(string(result.stdout)), nil
}

// createWorktree checks out a new branch ccl/<name> at the project's HEAD.
func (s *Server) createWorktree(ctx context.Context, ws Workspace, name string) (SessionWorktree, error) {
	if inside, err := s.gitText(ctx, ws, "rev-parse", "--is-inside-work-tree"); err != nil || inside != "true" {
		return SessionWorktree{}, errNotARepository
	}
	commit, err := s.gitText(ctx, ws, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || commit == "" {
		return SessionWorktree{}, errNoCommits
	}
	base, _ := s.gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	info := SessionWorktree{Name: name, Branch: "ccl/" + name, Base: base, BaseCommit: commit}
	dir := s.worktreeDir(ws, name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return SessionWorktree{}, err
	}
	if _, err := s.gitText(ctx, ws, "worktree", "add", "-b", info.Branch, "--", dir, commit); err != nil {
		return SessionWorktree{}, err
	}
	for _, rel := range worktreeLocalFiles {
		copyIfMissing(filepath.Join(ws.Path, rel), filepath.Join(dir, rel))
	}
	return info, nil
}

// copyIfMissing copies a regular file to a path that does not exist yet.
// Failing is harmless: the file is a convenience.
func copyIfMissing(from, to string) {
	info, err := os.Lstat(from)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(to), 0o755) == nil {
		_ = os.WriteFile(to, data, info.Mode().Perm())
	}
}

// countChanges counts the entries of git status --porcelain -z, leaving out
// the untracked personal files copied in at creation: they are never meant
// to be committed, so they must not block a merge.
func countChanges(porcelain []byte) int {
	count := 0
	entries := bytes.Split(porcelain, []byte{0})
	for i := 0; i < len(entries); i++ {
		entry := string(entries[i])
		if len(entry) < 4 {
			continue
		}
		// A rename or copy carries its old path as one more field.
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
		}
		if entry[:3] == "?? " && isLocalCopy(entry[3:]) {
			continue
		}
		count++
	}
	return count
}

func isLocalCopy(rel string) bool {
	for _, local := range worktreeLocalFiles {
		if filepath.ToSlash(local) == rel {
			return true
		}
	}
	return false
}

// removeWorktree deletes a conversation's worktree and, when it was merged,
// its branch. Uncommitted changes refuse unless force; an unmerged branch is
// always kept.
func (s *Server) removeWorktree(ctx context.Context, ws Workspace, info SessionWorktree, force bool) error {
	dir := s.worktreeDir(ws, info.Name)
	if _, err := os.Stat(dir); err == nil {
		// The personal files copied in are untracked, which git worktree
		// remove refuses; the project keeps the originals.
		for _, rel := range worktreeLocalFiles {
			if _, err := s.gitText(ctx, Workspace{Path: dir}, "ls-files", "--error-unmatch", "--", rel); err != nil {
				_ = os.Remove(filepath.Join(dir, rel))
			}
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if _, err := s.gitText(ctx, ws, append(args, "--", dir)...); err != nil {
			return err
		}
	}
	_, _ = s.gitText(ctx, ws, "worktree", "prune")
	_, _ = s.gitText(ctx, ws, "branch", "-d", "--", info.Branch)
	return nil
}

func (s *Server) worktreeStatus(ctx context.Context, ws Workspace, info SessionWorktree) WorktreeStatus {
	status := WorktreeStatus{SessionWorktree: info, Path: s.worktreeDir(ws, info.Name), Files: []WorktreeFile{}}
	status.Current, _ = s.gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	if remote, _ := s.gitText(ctx, ws, "config", "--get", "branch."+info.Base+".remote"); info.Base != "" && remote != "" && remote != "." {
		status.Remote = remote
	} else if remotes, _ := s.gitText(ctx, ws, "remote"); remotes != "" {
		status.Remote = strings.Fields(remotes)[0]
		for _, name := range strings.Fields(remotes) {
			if name == "origin" {
				status.Remote = name
			}
		}
	}
	tree, err := s.worktreeWorkspace(ws, info.Name)
	if err != nil {
		return status
	}
	status.Exists = true
	base := info.Base
	if base == "" {
		base = info.BaseCommit
	}
	if counts, err := s.gitText(ctx, tree, "rev-list", "--left-right", "--count", base+"...HEAD"); err == nil {
		if fields := strings.Fields(counts); len(fields) == 2 {
			status.Behind, _ = strconv.Atoi(fields[0])
			status.Ahead, _ = strconv.Atoi(fields[1])
		}
	}
	if result, err := s.git(ctx, tree, nil, "status", "--porcelain", "-z"); err == nil && result.code == 0 {
		status.Dirty = countChanges(result.stdout)
	}
	if result, err := s.git(ctx, tree, nil, "diff", "--name-status", "-z", "--no-renames", base+"...HEAD"); err == nil && result.code == 0 {
		fields := strings.Split(strings.TrimSuffix(string(result.stdout), "\x00"), "\x00")
		for i := 0; i+1 < len(fields) && len(status.Files) < maxWorktreeFiles; i += 2 {
			status.Files = append(status.Files, WorktreeFile{Status: fields[i], Path: fields[i+1]})
		}
	}
	status.Upstream, _ = s.gitText(ctx, tree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	return status
}

// handleWorktree serves /api/sessions/:id/worktree: GET the status, POST
// .../merge into the base branch, POST .../push to the remote.
func (s *Server) handleWorktree(w http.ResponseWriter, r *http.Request, session *Session, action string) {
	meta := session.Meta()
	if meta.Worktree == nil {
		writeError(w, http.StatusNotFound, "this conversation has no worktree")
		return
	}
	ws, ok := s.workspaces.byPath(meta.Workspace)
	if !ok {
		writeError(w, http.StatusConflict, ErrWorkspaceNotServed.Error())
		return
	}
	info := *meta.Worktree
	ctx := r.Context()
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.worktreeStatus(ctx, ws, info))
	case action == "diff" && r.Method == http.MethodGet:
		diff, err := s.sessionDiff(ctx, meta)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, diff)
	case (action == "merge" || action == "push") && r.Method == http.MethodPost:
		if s.timeline.Running(session.ID()) {
			writeError(w, http.StatusConflict, ErrBusy.Error())
			return
		}
		status := s.worktreeStatus(ctx, ws, info)
		if !status.Exists {
			writeError(w, http.StatusConflict, ErrWorktreeMissing.Error())
			return
		}
		var output string
		var err error
		if action == "merge" {
			output, err = s.mergeWorktree(ctx, ws, status)
		} else {
			output, err = s.pushWorktree(ctx, ws, status)
		}
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "output": output})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"output": output, "status": s.worktreeStatus(ctx, ws, info)})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// mergeWorktree merges the branch into its base in the project itself, which
// must have the base checked out with no uncommitted changes. A conflicted
// merge is aborted, so the project is never left half-merged.
func (s *Server) mergeWorktree(ctx context.Context, ws Workspace, status WorktreeStatus) (string, error) {
	switch {
	case status.Base == "":
		return "", errors.New("the worktree started from a detached HEAD; merge its branch yourself")
	case status.Current != status.Base:
		return "", fmt.Errorf("the project has %q checked out; check out %q there to merge into it", status.Current, status.Base)
	case status.Dirty > 0:
		return "", errors.New("the worktree has uncommitted changes; commit or discard them first")
	case status.Ahead == 0:
		return "", errors.New("the branch has no commits to merge")
	}
	if changes, err := s.gitText(ctx, ws, "status", "--porcelain", "--untracked-files=no"); err != nil || changes != "" {
		return "", errors.New("the project has uncommitted changes; commit or stash them first")
	}
	result, err := s.git(ctx, ws, nil, "merge", "--no-ff", "--no-edit", status.Branch)
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(string(result.stdout) + "\n" + result.stderr)
	if result.code != 0 {
		_, _ = s.git(ctx, ws, nil, "merge", "--abort")
		return output, errors.New("the merge did not apply cleanly and was aborted")
	}
	return output, nil
}

func (s *Server) pushWorktree(ctx context.Context, ws Workspace, status WorktreeStatus) (string, error) {
	if status.Remote == "" {
		return "", errors.New("the repository has no remote to push to")
	}
	tree, err := s.worktreeWorkspace(ws, status.Name)
	if err != nil {
		return "", err
	}
	result, err := s.git(ctx, tree, nil, "push", "--set-upstream", "--", status.Remote, status.Branch)
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(string(result.stdout) + "\n" + result.stderr)
	if result.code != 0 {
		return output, errors.New("git push failed")
	}
	return output, nil
}
