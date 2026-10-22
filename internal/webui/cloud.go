package webui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/cloudsync"
)

// Cloud sync from the page. Reading (remotes and their status) goes through
// internal/cloudsync directly; every change runs `ccl cloud …` (the server's
// own binary), so login, encryption and conflict rules stay in one place.
//
//	GET  /api/cloud                     remotes, each with its status
//	POST /api/cloud/login {platform, alias}
//	GET  /api/cloud/login/<id>          the login's output and URLs
//	POST /api/cloud/login/<id> {address} a pasted redirected address
//	DELETE /api/cloud/login/<id>        cancel
//	POST /api/cloud/push  {remote, force}
//	POST /api/cloud/pull  {remote, force}
//	POST /api/cloud/primary {remote}
//	POST /api/cloud/logout  {remote}

// cloudCommandTimeout bounds a push, pull or remote change.
const cloudCommandTimeout = 2 * time.Minute

// cloudStatusTimeout bounds reading one remote's status, which may ask the
// provider for its latest snapshot.
const cloudStatusTimeout = 20 * time.Second

// CloudRemote is one configured remote with its status, as the page sees it.
type CloudRemote struct {
	Alias       string `json:"alias"`
	Provider    string `json:"provider"`
	Primary     bool   `json:"primary"`
	Mirror      bool   `json:"mirror"`
	SignedIn    bool   `json:"signedIn"`
	AccountHint string `json:"accountHint,omitempty"`
	// State is cloudsync's: up to date, local changes pending, …
	State          string    `json:"state,omitempty"`
	LocalHash      string    `json:"localHash,omitempty"`
	RemoteTag      string    `json:"remoteTag,omitempty"`
	RemoteDeviceID string    `json:"remoteDevice,omitempty"`
	RemoteCreated  time.Time `json:"remoteCreated,omitempty"`
	DeviceID       string    `json:"device,omitempty"`
	LastOperation  string    `json:"lastOperation,omitempty"`
	LastSyncAt     time.Time `json:"lastSyncAt,omitempty"`
	Error          string    `json:"error,omitempty"`
}

func (s *Server) handleCloud(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/cloud"), "/")
	action, id, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.cloudOverview(w)
	case action == "login" && id == "" && r.Method == http.MethodPost:
		s.cloudLogin(w, r)
	case action == "login" && id != "":
		s.cloudLoginProgress(w, r, id)
	case (action == "push" || action == "pull" || action == "primary" || action == "logout") && id == "" && r.Method == http.MethodPost:
		s.cloudCommand(w, r, action)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) cloudOverview(w http.ResponseWriter) {
	infos, err := cloudsync.ListRemotes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	remotes := make([]CloudRemote, 0, len(infos))
	for _, info := range infos {
		remote := CloudRemote{
			Alias: info.Alias, Provider: info.Provider, Primary: info.Primary, Mirror: info.Mirror,
			SignedIn: info.SignedIn, AccountHint: info.AccountHint,
		}
		if info.SignedIn {
			status, err := remoteStatus(info.Alias)
			if err != nil {
				remote.Error = err.Error()
			} else {
				remote.State, remote.LocalHash, remote.RemoteTag = status.State, status.LocalHash, status.RemoteTag
				remote.RemoteDeviceID, remote.RemoteCreated, remote.DeviceID = status.RemoteDeviceID, status.RemoteCreated, status.DeviceID
				remote.LastOperation, remote.LastSyncAt = status.LastOperation, status.LastSyncAt
			}
		}
		remotes = append(remotes, remote)
	}
	platforms := []string{"google-drive"}
	if runtime.GOOS == "darwin" {
		platforms = append(platforms, "icloud")
	}
	writeJSON(w, http.StatusOK, map[string]any{"remotes": remotes, "platforms": platforms, "available": s.selfBinary != ""})
}

// remoteStatus reads one remote's status, giving up after cloudStatusTimeout:
// a provider that does not answer must not hold the settings page.
func remoteStatus(alias string) (cloudsync.Status, error) {
	type result struct {
		status cloudsync.Status
		err    error
	}
	done := make(chan result, 1)
	go func() {
		manager, err := cloudsync.LoadRemote(alias)
		if err != nil {
			done <- result{err: err}
			return
		}
		status, err := manager.Status()
		done <- result{status, err}
	}()
	select {
	case got := <-done:
		return got.status, got.err
	case <-time.After(cloudStatusTimeout):
		return cloudsync.Status{}, errors.New("the cloud provider did not answer in time")
	}
}

func (s *Server) cloudLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Platform string `json:"platform"`
		Alias    string `json:"alias"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	platform := strings.TrimSpace(body.Platform)
	if platform != "google-drive" && !(platform == "icloud" && runtime.GOOS == "darwin") {
		writeError(w, http.StatusBadRequest, "unsupported cloud platform "+platform)
		return
	}
	alias := strings.TrimSpace(body.Alias)
	if alias != "" && !aliasPattern.MatchString(alias) {
		writeError(w, http.StatusBadRequest, "invalid remote name")
		return
	}
	if s.selfBinary == "" {
		writeError(w, http.StatusNotImplemented, "cloud sync is not available in this build")
		return
	}
	args := []string{"cloud", "login", platform}
	if alias != "" {
		args = append(args, alias)
	}
	// The page shows the URL; a browser on this machine completes the login
	// through ccl's own 127.0.0.1 callback, any other pastes the address back.
	if platform == "google-drive" {
		args = append(args, "--no-browser")
	}
	login, err := s.runLogin(platform, args, platform == "google-drive")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": login.ID})
}

// cloudLoginProgress serves a cloud login like a subscription login, plus
// POST {address} for the redirected address pasted from another device.
func (s *Server) cloudLoginProgress(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		// GET and DELETE are the subscription login's.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api/oauth/login/" + id
		s.handleOAuth(w, r2)
		return
	}
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	address := strings.TrimSpace(body.Address)
	// One line, and only the redirected address: ccl reads its query.
	if address == "" || strings.ContainsAny(address, "\r\n") || len(address) > 4096 {
		writeError(w, http.StatusBadRequest, "paste the whole redirected 127.0.0.1 address")
		return
	}
	s.logins.mu.Lock()
	login, ok := s.logins.logins[id]
	waiting := ok && login.stdin != nil && !login.Done
	s.logins.mu.Unlock()
	if !waiting {
		writeError(w, http.StatusConflict, "this login is not waiting for an address")
		return
	}
	if _, err := login.stdin.Write([]byte(address + "\n")); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cloudCommand runs push, pull, primary (remote use) or logout and returns
// what ccl printed. A refusal (a conflict, local changes) is 409 with the
// output, so the page can offer --force.
func (s *Server) cloudCommand(w http.ResponseWriter, r *http.Request, action string) {
	var body struct {
		Remote string `json:"remote"`
		Force  bool   `json:"force"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	remote := strings.TrimSpace(body.Remote)
	if remote != "" && !aliasPattern.MatchString(remote) {
		writeError(w, http.StatusBadRequest, "invalid remote name")
		return
	}
	if (action == "primary" || action == "logout") && remote == "" {
		writeError(w, http.StatusBadRequest, "remote is required")
		return
	}
	if s.selfBinary == "" {
		writeError(w, http.StatusNotImplemented, "cloud sync is not available in this build")
		return
	}
	var args []string
	switch action {
	case "push":
		args = []string{"cloud", "push"}
		if remote != "" {
			args = append(args, "--to", remote)
		}
	case "pull":
		args = []string{"cloud", "pull"}
		if remote != "" {
			args = append(args, "--from", remote)
		}
	case "primary":
		args = []string{"cloud", "remote", "use", remote}
	case "logout":
		// Local only: the encrypted data and the profile key stay.
		args = []string{"cloud", "logout", remote}
	}
	if body.Force && (action == "push" || action == "pull") {
		args = append(args, "--force")
	}
	ctx, cancel := context.WithTimeout(r.Context(), cloudCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.selfBinary, args...)
	if workspace, ok := s.workspaces.fallback(); ok {
		cmd.Dir = workspace.Path
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	text := redactSecrets(strings.TrimSpace(output.String()))
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		writeJSON(w, http.StatusConflict, map[string]any{"error": lastLine(text, "ccl cloud "+action+" failed"), "output": text})
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"output": text})
	}
}

// lastLine is a command's final message, which says why it failed.
func lastLine(text, fallback string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return strings.TrimPrefix(last, "Error: ")
	}
	return fallback
}
