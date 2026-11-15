//go:build windows

package webui

import (
	"net/http"
	"sync"
)

// The web terminal needs a Unix pseudo-terminal; on Windows it says so.

type terminal struct{}

type terminalRegistry struct {
	mu        sync.Mutex
	terminals map[string]*terminal
}

func (s *Server) closeTerminals() {}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	writeError(w, http.StatusNotImplemented, "the web terminal is not available on Windows")
}
