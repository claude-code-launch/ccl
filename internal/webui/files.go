package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// The workspace file API lets the page browse, read, edit and search the
// files of --workspace, and nothing outside it.

const (
	// MaxReadBytes is the largest file returned as text for viewing or editing.
	MaxReadBytes = 2 << 20
	// MaxWriteBytes bounds a file saved from the editor.
	MaxWriteBytes = 5 << 20
	searchLimit   = 50
	// searchWalkLimit bounds how many entries a search without git visits.
	searchWalkLimit = 50000
)

// FileEntry is one directory entry.
type FileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"` // file, dir or symlink
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// FileContent is a file as the editor sees it.
type FileContent struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Hash is the content's sha256; a save names the hash it started from.
	Hash   string `json:"hash"`
	Type   string `json:"type"`
	Binary bool   `json:"binary"`
	// Image marks a file the page can show through the raw endpoint.
	Image   bool   `json:"image"`
	TooBig  bool   `json:"tooBig,omitempty"`
	Content string `json:"content,omitempty"`
}

var errOutsideWorkspace = errors.New("path is outside the workspace")

// resolve maps a workspace-relative path to an absolute one inside the
// workspace. Symbolic links are followed before the check, so a link cannot
// lead outside; a path that does not exist yet is checked through its parent.
func (ws Workspace) resolve(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", errOutsideWorkspace
	}
	clean := path.Clean("/" + filepath.ToSlash(rel))
	abs := filepath.Join(ws.Path, filepath.FromSlash(clean))
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		// Resolve the deepest ancestor that exists; the rest is created later
		// and cannot be a link yet.
		existing, missing := abs, ""
		for errors.Is(err, fs.ErrNotExist) && existing != filepath.Dir(existing) {
			missing = filepath.Join(filepath.Base(existing), missing)
			existing = filepath.Dir(existing)
			real, err = filepath.EvalSymlinks(existing)
		}
		if err != nil {
			return "", err
		}
		real = filepath.Join(real, missing)
	} else if err != nil {
		return "", err
	}
	if !withinWorkspace(real, ws.real) {
		return "", errOutsideWorkspace
	}
	return real, nil
}

// relative is how the page names a path: slash-separated, from the workspace.
func (ws Workspace) relative(abs string) string {
	rel, err := filepath.Rel(ws.real, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request, ws Workspace) {
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/files"), "/")
	switch {
	case action == "list" && r.Method == http.MethodGet:
		s.listFiles(w, r, ws)
	case action == "read" && r.Method == http.MethodGet:
		s.readFile(w, r, ws)
	case action == "write" && r.Method == http.MethodPut:
		s.writeFile(w, r, ws)
	case action == "search" && r.Method == http.MethodGet:
		s.searchFiles(w, r, ws)
	case action == "uploads" && r.Method == http.MethodPost:
		s.uploadFiles(w, r, ws)
	case action == "list" || action == "read" || action == "write" || action == "search":
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOutsideWorkspace):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "no such file or directory")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, ws Workspace) {
	dir, err := ws.resolve(r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.fileError(w, err)
		return
	}
	list := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		// Git's own store is never something to browse or edit.
		if entry.Name() == ".git" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		kind := "file"
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
			// A link to a directory inside the workspace browses like one.
			if target, err := ws.resolve(ws.relative(filepath.Join(dir, entry.Name()))); err == nil {
				if stat, err := os.Stat(target); err == nil && stat.IsDir() {
					kind = "dir"
				}
			}
		case entry.IsDir():
			kind = "dir"
		}
		list = append(list, FileEntry{
			Name:     entry.Name(),
			Path:     ws.relative(filepath.Join(dir, entry.Name())),
			Type:     kind,
			Size:     info.Size(),
			Modified: info.ModTime().UTC(),
		})
	}
	sort.Slice(list, func(i, j int) bool {
		if (list[i].Type == "dir") != (list[j].Type == "dir") {
			return list[i].Type == "dir"
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": ws.relative(dir), "entries": list})
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request, ws Workspace) {
	file, err := ws.resolve(r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, err)
		return
	}
	info, err := os.Stat(file)
	if err != nil {
		s.fileError(w, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	mediaType := detectType(file, file)
	content := FileContent{
		Path:     ws.relative(file),
		Size:     info.Size(),
		Modified: info.ModTime().UTC(),
		Type:     mediaType,
		Image:    inlineImageTypes[mediaType],
	}
	if info.Size() > MaxReadBytes {
		content.TooBig = true
		content.Binary = !looksLikeText(file)
		writeJSON(w, http.StatusOK, content)
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		s.fileError(w, err)
		return
	}
	sum := sha256.Sum256(data)
	content.Hash = hex.EncodeToString(sum[:])
	if isBinary(data) {
		content.Binary = true
	} else {
		content.Content = string(data)
	}
	writeJSON(w, http.StatusOK, content)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request, ws Workspace) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		// ExpectedHash is the hash the editor loaded. A different file on disk
		// means someone (usually Claude) changed it since: the save is refused.
		// Empty means the file must not exist yet.
		ExpectedHash string `json:"expectedHash"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxWriteBytes+(64<<10))
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Content) > MaxWriteBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file is larger than %d MB", MaxWriteBytes>>20))
		return
	}
	if strings.Trim(path.Clean("/"+body.Path), "/") == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	file, err := ws.resolve(body.Path)
	if err != nil {
		s.fileError(w, err)
		return
	}
	mode := fs.FileMode(0o644)
	current, err := os.ReadFile(file)
	switch {
	case err == nil:
		sum := sha256.Sum256(current)
		if hash := hex.EncodeToString(sum[:]); hash != body.ExpectedHash {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "the file changed on disk since it was opened", "hash": hash})
			return
		}
		if info, err := os.Stat(file); err == nil {
			mode = info.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
		if body.ExpectedHash != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "the file was deleted since it was opened"})
			return
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			s.fileError(w, err)
			return
		}
	default:
		s.fileError(w, err)
		return
	}
	if err := fsutil.WriteFileAtomic(file, []byte(body.Content), mode); err != nil {
		s.fileError(w, err)
		return
	}
	sum := sha256.Sum256([]byte(body.Content))
	writeJSON(w, http.StatusOK, map[string]string{"path": ws.relative(file), "hash": hex.EncodeToString(sum[:])})
}

// searchFiles finds files by name for @ mentions. In a git repository it asks
// git, which honors .gitignore; elsewhere it walks the tree.
func (s *Server) searchFiles(w http.ResponseWriter, r *http.Request, ws Workspace) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	paths, err := s.gitFiles(r.Context(), ws)
	if err != nil {
		paths = ws.walkFiles()
	}
	type match struct {
		path string
		rank int
	}
	matches := []match{}
	for _, candidate := range paths {
		lower := strings.ToLower(candidate)
		base := path.Base(lower)
		rank := -1
		switch {
		case query == "":
			rank = 3
		case strings.HasPrefix(base, query):
			rank = 0
		case strings.Contains(base, query):
			rank = 1
		case strings.Contains(lower, query):
			rank = 2
		}
		if rank >= 0 {
			matches = append(matches, match{candidate, rank})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return len(matches[i].path) < len(matches[j].path)
	})
	results := []string{}
	for _, m := range matches {
		if len(results) == searchLimit {
			break
		}
		results = append(results, m.path)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": results})
}

func (s *Server) gitFiles(ctx context.Context, ws Workspace) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = ws.Path
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, item := range bytes.Split(out, []byte{0}) {
		if len(item) > 0 {
			paths = append(paths, string(item))
		}
	}
	return paths, nil
}

func (ws Workspace) walkFiles() []string {
	var paths []string
	visited := 0
	_ = filepath.WalkDir(ws.real, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > searchWalkLimit {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, ws.relative(file))
		return nil
	})
	return paths
}

// serveRaw returns a workspace file for <img> and downloads, with the same
// rules as attachments: only plain image types inline, nothing runs here.
func (s *Server) serveRaw(w http.ResponseWriter, r *http.Request, ws Workspace) {
	file, err := ws.resolve(r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, err)
		return
	}
	info, err := os.Stat(file)
	if err != nil || info.IsDir() {
		s.fileError(w, fs.ErrNotExist)
		return
	}
	serveSafely(w, file, filepath.Base(file), detectType(file, file), "no-cache")
}

func serveSafely(w http.ResponseWriter, file, name, mediaType, cacheControl string) {
	handle, err := os.Open(file)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer handle.Close()
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", cacheControl)
	if inlineImageTypes[mediaType] {
		header.Set("Content-Type", mediaType)
	} else {
		header.Set("Content-Type", "application/octet-stream")
		header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	_, _ = io.Copy(w, handle)
}

func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(data)
}

func looksLikeText(file string) bool {
	handle, err := os.Open(file)
	if err != nil {
		return false
	}
	defer handle.Close()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(bufio.NewReader(handle), head)
	return bytes.IndexByte(head[:n], 0) < 0
}

// uploadFiles stores files dropped onto the file tree into a workspace
// directory. An existing file is never replaced.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request, ws Workspace) {
	dir, err := ws.resolve(r.URL.Query().Get("dir"))
	if err != nil {
		s.fileError(w, err)
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "target is not a directory")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequest)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	saved := []string{}
	for _, header := range r.MultipartForm.File["file"] {
		if header.Size > MaxUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is larger than %d MB", header.Filename, MaxUploadBytes>>20))
			return
		}
		target := filepath.Join(dir, sanitizeFileName(header.Filename))
		// The name came from the browser: check it resolves inside as well.
		if checked, err := ws.resolve(ws.relative(target)); err != nil || checked != target {
			writeError(w, http.StatusForbidden, errOutsideWorkspace.Error())
			return
		}
		content, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			content.Close()
			if errors.Is(err, fs.ErrExist) {
				writeError(w, http.StatusConflict, filepath.Base(target)+" already exists")
				return
			}
			s.fileError(w, err)
			return
		}
		_, err = io.Copy(file, content)
		content.Close()
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(target)
			s.fileError(w, err)
			return
		}
		saved = append(saved, ws.relative(target))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"files": saved})
}
