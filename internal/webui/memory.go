package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// grokConfigDir is where Grok Build keeps its user files: GROK_HOME, else ~/.grok.
func grokConfigDir() string {
	if dir := os.Getenv("GROK_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// UserMemory is the user's own AGENTS.md in ~/.grok, which Grok Build reads in
// every project.
type UserMemory struct {
	Path string `json:"path"`
	// Hash is empty when the file does not exist yet.
	Hash    string `json:"hash"`
	Content string `json:"content"`
	TooBig  bool   `json:"tooBig,omitempty"`
}

// handleMemory reads (GET) and saves (PUT {content, expectedHash}) the user
// AGENTS.md. Only that one file: project files go through /api/files.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	dir := grokConfigDir()
	if dir == "" {
		writeError(w, http.StatusInternalServerError, "no home directory")
		return
	}
	file := filepath.Join(dir, "AGENTS.md")
	switch r.Method {
	case http.MethodGet:
		memory := UserMemory{Path: file}
		data, err := os.ReadFile(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			s.fileError(w, err)
			return
		case len(data) > MaxReadBytes:
			memory.TooBig = true
		default:
			sum := sha256.Sum256(data)
			memory.Hash = hex.EncodeToString(sum[:])
			memory.Content = string(data)
		}
		writeJSON(w, http.StatusOK, memory)
	case http.MethodPut:
		var body struct {
			Content      string `json:"content"`
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
		hash, err := saveVerified(file, []byte(body.Content), body.ExpectedHash)
		var stale *staleFileError
		switch {
		case errors.As(err, &stale):
			writeStale(w, stale)
		case err != nil:
			s.fileError(w, err)
		default:
			writeJSON(w, http.StatusOK, map[string]string{"path": file, "hash": hash})
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
