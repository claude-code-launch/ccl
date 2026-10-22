package webui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The git API backs the page's changes panel: status, diffs, staging,
// commits, branches and history of --workspace. git runs from argv, never a
// shell, and every path is checked against the workspace first.

const maxDiffBytes = 1 << 20

// GitFile is one changed path. Index and Worktree are porcelain status
// letters ('.' for unchanged).
type GitFile struct {
	Path       string `json:"path"`
	OrigPath   string `json:"origPath,omitempty"`
	Index      string `json:"index"`
	Worktree   string `json:"worktree"`
	Untracked  bool   `json:"untracked,omitempty"`
	Conflicted bool   `json:"conflicted,omitempty"`
	Added      int    `json:"added"`
	Deleted    int    `json:"deleted"`
}

// GitStatus is the working tree as the changes panel shows it.
type GitStatus struct {
	Repo     bool      `json:"repo"`
	Branch   string    `json:"branch,omitempty"`
	Detached bool      `json:"detached,omitempty"`
	Upstream string    `json:"upstream,omitempty"`
	Ahead    int       `json:"ahead"`
	Behind   int       `json:"behind"`
	Files    []GitFile `json:"files"`
	Added    int       `json:"added"`
	Deleted  int       `json:"deleted"`
	Error    string    `json:"error,omitempty"`
}

// GitCommit is one history entry.
type GitCommit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

type gitResult struct {
	stdout []byte
	stderr string
	code   int
}

func (s *Server) git(ctx context.Context, stdin []byte, args ...string) (gitResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotepath=off", "-c", "color.ui=false"}, args...)...)
	cmd.Dir = s.workspace
	// Reads must not take the index lock Claude's own git commands need.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := gitResult{stdout: stdout.Bytes(), stderr: strings.TrimSpace(stderr.String())}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.code = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/git"), "/")
	get := r.Method == http.MethodGet
	post := r.Method == http.MethodPost
	switch {
	case action == "status" && get:
		writeJSON(w, http.StatusOK, s.gitStatus(r.Context()))
	case action == "diff" && get:
		s.gitDiff(w, r)
	case action == "stage" && post:
		s.gitPaths(w, r, "add", "--")
	case action == "unstage" && post:
		s.gitPaths(w, r, "restore", "--staged", "--")
	case action == "commit" && post:
		s.gitCommit(w, r)
	case action == "branches" && get:
		s.gitBranches(w, r)
	case action == "checkout" && post:
		s.gitCheckout(w, r)
	case action == "log" && get:
		s.gitLog(w, r)
	case action == "show" && get:
		s.gitShow(w, r)
	case action == "worktrees" && get:
		s.gitWorktrees(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) gitStatus(ctx context.Context) GitStatus {
	status := GitStatus{Files: []GitFile{}}
	result, err := s.git(ctx, nil, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		status.Error = "git is not available: " + err.Error()
		return status
	}
	if result.code != 0 {
		// Not a repository is a normal state for a workspace.
		return status
	}
	status.Repo = true
	parseStatus(result.stdout, &status)

	// Line counts: tracked changes against HEAD (or the index in a repository
	// without commits), and untracked files as all-added.
	counts := map[string][2]int{}
	numstat, err := s.git(ctx, nil, "diff", "--numstat", "-z", "HEAD")
	if err == nil && numstat.code != 0 {
		numstat, err = s.git(ctx, nil, "diff", "--numstat", "-z", "--cached")
	}
	if err == nil && numstat.code == 0 {
		parseNumstat(numstat.stdout, counts)
	}
	for i, file := range status.Files {
		if file.Untracked {
			if lines, ok := countLines(s, file.Path); ok {
				counts[file.Path] = [2]int{lines, 0}
			}
		}
		if count, ok := counts[file.Path]; ok {
			status.Files[i].Added, status.Files[i].Deleted = count[0], count[1]
			status.Added += count[0]
			status.Deleted += count[1]
		}
	}
	return status
}

func parseStatus(out []byte, status *GitStatus) {
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		switch {
		case strings.HasPrefix(record, "# branch.head "):
			status.Branch = strings.TrimPrefix(record, "# branch.head ")
			if status.Branch == "(detached)" {
				status.Detached = true
			}
		case strings.HasPrefix(record, "# branch.upstream "):
			status.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
		case strings.HasPrefix(record, "# branch.ab "):
			fields := strings.Fields(strings.TrimPrefix(record, "# branch.ab "))
			if len(fields) == 2 {
				status.Ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
				status.Behind, _ = strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
			}
		case strings.HasPrefix(record, "1 "), strings.HasPrefix(record, "u "):
			// 1 XY sub mH mI mW hH hI path / u XY sub m1 m2 m3 mW h1 h2 h3 path
			fields := 9
			if record[0] == 'u' {
				fields = 11
			}
			parts := strings.SplitN(record, " ", fields)
			if len(parts) == fields {
				status.Files = append(status.Files, GitFile{
					Path: parts[fields-1], Index: parts[1][:1], Worktree: parts[1][1:],
					Conflicted: record[0] == 'u',
				})
			}
		case strings.HasPrefix(record, "2 "):
			// 2 XY sub mH mI mW hH hI Xscore path, then the original path.
			parts := strings.SplitN(record, " ", 10)
			if len(parts) == 10 {
				file := GitFile{Path: parts[9], Index: parts[1][:1], Worktree: parts[1][1:]}
				if i+1 < len(records) {
					file.OrigPath = records[i+1]
					i++
				}
				status.Files = append(status.Files, file)
			}
		case strings.HasPrefix(record, "? "):
			status.Files = append(status.Files, GitFile{Path: record[2:], Index: "?", Worktree: "?", Untracked: true})
		}
	}
}

func parseNumstat(out []byte, counts map[string][2]int) {
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		fields := strings.SplitN(records[i], "\t", 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if path == "" && i+2 < len(records) {
			// A rename: the old and new paths follow as their own records.
			path = records[i+2]
			i += 2
		}
		added, _ := strconv.Atoi(fields[0]) // "-" for binary files counts as 0
		deleted, _ := strconv.Atoi(fields[1])
		counts[path] = [2]int{added, deleted}
	}
}

func countLines(s *Server, rel string) (int, bool) {
	file, err := s.resolve(rel)
	if err != nil {
		return 0, false
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() > MaxReadBytes {
		return 0, false
	}
	data, err := os.ReadFile(file)
	if err != nil || isBinary(data) {
		return 0, false
	}
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines, true
}

// checkedPath validates a path from the page and returns it relative to the
// workspace, as git takes it.
func (s *Server) checkedPath(rel string) (string, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	if relative := s.relative(abs); relative != "" {
		return relative, nil
	}
	return ".", nil
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	args := []string{"diff", "--no-ext-diff", "--no-color"}
	if query.Get("staged") == "1" {
		args = append(args, "--cached")
	}
	if rel := query.Get("path"); rel != "" {
		path, err := s.checkedPath(rel)
		if err != nil {
			s.fileError(w, err)
			return
		}
		if query.Get("untracked") == "1" {
			// An untracked file diffs against nothing; exit status 1 means "differs".
			args = []string{"diff", "--no-ext-diff", "--no-color", "--no-index", "--", "/dev/null", path}
		} else {
			args = append(args, "--", path)
		}
	}
	result, err := s.git(r.Context(), nil, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result.code > 1 {
		writeError(w, http.StatusBadRequest, result.stderr)
		return
	}
	diff, truncated := result.stdout, false
	if len(diff) > maxDiffBytes {
		diff, truncated = diff[:maxDiffBytes], true
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": string(diff), "truncated": truncated})
}

func (s *Server) gitPaths(w http.ResponseWriter, r *http.Request, args ...string) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "paths are required")
		return
	}
	for _, rel := range body.Paths {
		path, err := s.checkedPath(rel)
		if err != nil {
			s.fileError(w, err)
			return
		}
		args = append(args, path)
	}
	result, err := s.git(r.Context(), nil, args...)
	if err == nil && result.code != 0 && args[0] == "restore" {
		// A repository without commits has nothing to restore from.
		result, err = s.git(r.Context(), nil, append([]string{"rm", "--cached", "-r", "--quiet", "--"}, args[3:]...)...)
	}
	s.gitDone(w, r, result, err)
}

func (s *Server) gitDone(w http.ResponseWriter, r *http.Request, result gitResult, err error) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result.code != 0 {
		writeError(w, http.StatusBadRequest, firstNonEmpty(result.stderr, strings.TrimSpace(string(result.stdout)), "git failed"))
		return
	}
	writeJSON(w, http.StatusOK, s.gitStatus(r.Context()))
}

func (s *Server) gitCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "a commit message is required")
		return
	}
	// The message travels on stdin, so no part of it is ever read as an option.
	result, err := s.git(r.Context(), []byte(body.Message), "commit", "--file=-")
	s.gitDone(w, r, result, err)
}

func (s *Server) gitBranches(w http.ResponseWriter, r *http.Request) {
	result, err := s.git(r.Context(), nil, "for-each-ref", "--format=%(refname:short)%00%(HEAD)%00%(upstream:short)%00%(committerdate:iso-strict)", "refs/heads")
	if err != nil || result.code != 0 {
		writeJSON(w, http.StatusOK, map[string]any{"branches": []any{}})
		return
	}
	type branch struct {
		Name     string `json:"name"`
		Current  bool   `json:"current"`
		Upstream string `json:"upstream,omitempty"`
		Date     string `json:"date,omitempty"`
	}
	branches := []branch{}
	for _, line := range strings.Split(strings.TrimSpace(string(result.stdout)), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) == 4 && fields[0] != "" {
			branches = append(branches, branch{Name: fields[0], Current: fields[1] == "*", Upstream: fields[2], Date: fields[3]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func (s *Server) gitCheckout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Branch string `json:"branch"`
		Create bool   `json:"create"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Branch)
	if name == "" || strings.HasPrefix(name, "-") {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	if check, err := s.git(r.Context(), nil, "check-ref-format", "--branch", name); err != nil || check.code != 0 {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	args := []string{"switch"}
	if body.Create {
		args = append(args, "--create")
	}
	result, err := s.git(r.Context(), nil, append(args, name)...)
	s.gitDone(w, r, result, err)
}

func (s *Server) gitLog(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	result, err := s.git(r.Context(), nil, "log", "-n", strconv.Itoa(limit), "--format=%H%x00%h%x00%an%x00%aI%x00%s%x1e")
	commits := []GitCommit{}
	if err == nil && result.code == 0 {
		for _, record := range strings.Split(string(result.stdout), "\x1e") {
			fields := strings.Split(strings.TrimSpace(record), "\x00")
			if len(fields) == 5 {
				commits = append(commits, GitCommit{Hash: fields[0], Short: fields[1], Author: fields[2], Date: fields[3], Subject: fields[4]})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": commits})
}

func (s *Server) gitShow(w http.ResponseWriter, r *http.Request) {
	commit := r.URL.Query().Get("commit")
	if !commitPattern.MatchString(commit) {
		writeError(w, http.StatusBadRequest, "invalid commit")
		return
	}
	result, err := s.git(r.Context(), nil, "show", "--no-ext-diff", "--no-color", "--stat", "--patch", commit, "--")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result.code != 0 {
		writeError(w, http.StatusNotFound, firstNonEmpty(result.stderr, "commit not found"))
		return
	}
	diff, truncated := result.stdout, false
	if len(diff) > maxDiffBytes {
		diff, truncated = diff[:maxDiffBytes], true
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": string(diff), "truncated": truncated})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Server) gitWorktrees(w http.ResponseWriter, r *http.Request) {
	type worktree struct {
		Path     string `json:"path"`
		Branch   string `json:"branch,omitempty"`
		Head     string `json:"head,omitempty"`
		Detached bool   `json:"detached,omitempty"`
		Current  bool   `json:"current,omitempty"`
	}
	trees := []worktree{}
	result, err := s.git(r.Context(), nil, "worktree", "list", "--porcelain")
	if err == nil && result.code == 0 {
		var current *worktree
		for _, line := range strings.Split(string(result.stdout), "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				trees = append(trees, worktree{Path: strings.TrimPrefix(line, "worktree ")})
				current = &trees[len(trees)-1]
				current.Current = sameDir(current.Path, s.realWorkspace)
			case current != nil && strings.HasPrefix(line, "HEAD "):
				current.Head = strings.TrimPrefix(line, "HEAD ")
			case current != nil && strings.HasPrefix(line, "branch "):
				current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			case current != nil && line == "detached":
				current.Detached = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": trees})
}

func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	return ra == b
}
