// Package proxymanager owns a standalone Codex provider runtime and reversible
// Codex configuration overlay. Its control API only listens on loopback.
package proxymanager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

type State struct {
	ProxyVersion int    `json:"proxy_version,omitempty"`
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	PID          int    `json:"pid"`
	BaseURL      string `json:"base_url"`
	ControlURL   string `json:"control_url"`
	ControlToken string `json:"control_token"`
	// Legacy Claude overlay fields are read only to restore pre-Codex-only daemons.
	SettingsPath  string          `json:"settings_path,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	ConfigHash    string          `json:"config_hash"`
	MetadataStamp string          `json:"metadata_stamp,omitempty"`
	CodexMode     string          `json:"codex_mode,omitempty"`
	EnvBefore     json.RawMessage `json:"env_before,omitempty"`
	Changes       []Change        `json:"changes,omitempty"`
	CodexHome     string          `json:"codex_home,omitempty"`
	Codex         *codex.Overlay  `json:"codex,omitempty"`
}

type Manager struct {
	Dir string
	// CodexListenAddress pins replacements, including a failed menu switch rollback.
	CodexListenAddress string
	// Empty inherits the running daemon's mode, or uses shared on first start.
	CodexMode string
}

// Bump when an existing daemon must be replaced to pick up a protocol fix.
// Version 15 adds live catalog refresh alongside the tool-name protocol fixes.
const proxyVersion = 15

func Default() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Manager{Dir: filepath.Join(home, ".ccl", "proxy")}, nil
}

func (m *Manager) statePath() string { return filepath.Join(m.Dir, "state.json") }

func (m *Manager) Read() (*State, error) {
	data, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("read proxy state: %w", err)
	}
	if s.ID == "" || (s.Codex == nil && s.SettingsPath == "") {
		return nil, fmt.Errorf("incomplete proxy state; recovery journal retained at %s", m.statePath())
	}
	return &s, nil
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	return fsutil.AcquireFileLock(ctx, filepath.Join(m.Dir, "command.lock"))
}

func control(ctx context.Context, s *State, method, path string) error {
	u, err := url.Parse(s.ControlURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil {
		return fmt.Errorf("invalid local proxy control endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, method, s.ControlURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.ControlToken)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(data) != s.ID {
		return fmt.Errorf("proxy control identity mismatch")
	}
	return nil
}

func (m *Manager) Running(ctx context.Context, s *State) bool {
	return s != nil && control(ctx, s, http.MethodGet, "/health") == nil
}

func (m *Manager) Stop(ctx context.Context) error {
	release, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return m.stop(ctx)
}

// RecoverStale restores the settings overlay of a proxy that died without
// cleaning up (kill -9, power loss), including legacy Claude overlays.
// It never waits: a start or stop in progress owns the command
// lock and recovers on its own, and a held daemon lock means the proxy is
// alive. Reports whether it restored anything.
func (m *Manager) RecoverStale(ctx context.Context) (bool, error) {
	if _, err := os.Stat(m.statePath()); err != nil {
		return false, nil
	}
	releaseCommand, ok, err := fsutil.TryFileLock(filepath.Join(m.Dir, "command.lock"))
	if err != nil || !ok {
		return false, err
	}
	defer releaseCommand()
	s, err := m.Read()
	if err != nil || s == nil {
		return false, err
	}
	releaseDaemon, ok, err := fsutil.TryFileLock(filepath.Join(m.Dir, "daemon.lock"))
	if err != nil || !ok {
		return false, err
	}
	defer releaseDaemon()
	if err := restoreState(ctx, s); err != nil {
		return false, fmt.Errorf("restore client settings: %w", err)
	}
	return true, os.Remove(m.statePath())
}

func (m *Manager) stop(ctx context.Context) error {
	s, err := m.Read()
	if err != nil || s == nil {
		return err
	}
	if m.Running(ctx, s) {
		// The peer may finish shutdown while its response is being read. The
		// lifecycle lock below is the authority on whether it actually stopped.
		_ = control(ctx, s, http.MethodPost, "/stop")
	}
	// The lifecycle lock is held until runtime shutdown and settings restoration
	// finish. Waiting for it also distinguishes a dead process from a temporarily
	// unreachable control socket, without ever signalling a reused PID.
	release, err := fsutil.AcquireFileLock(ctx, filepath.Join(m.Dir, "daemon.lock"))
	if err != nil {
		return fmt.Errorf("wait for proxy shutdown: %w", err)
	}
	defer release()
	current, err := m.Read()
	if err != nil || current == nil {
		return err
	}
	if current.ID != s.ID {
		return fmt.Errorf("proxy changed during shutdown")
	}
	if err := restoreState(ctx, current); err != nil {
		return fmt.Errorf("restore client settings: %w", err)
	}
	return os.Remove(m.statePath())
}

func randomID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// Start serializes starts and provider switches. The child is detached from
// the launching terminal and reports readiness only after the settings apply.
func (m *Manager) Start(ctx context.Context, executable string, p provider.Provider) (*State, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	existing, err := m.Read()
	if err != nil {
		return nil, err
	}
	mode := m.CodexMode
	if mode == "" && existing != nil {
		mode = existing.CodexMode
	}
	if mode == "" {
		mode = "shared"
	}
	if mode != "shared" && mode != "independent" {
		return nil, fmt.Errorf("invalid Codex proxy mode")
	}
	codexHome, err := codex.Home()
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(codexHome); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("Codex profile not found; open Codex once before starting its proxy")
	}
	if m.Running(ctx, existing) && existing.ConfigHash == providerHash(p) &&
		(existing.CodexMode == mode || existing.CodexMode == "" && mode == "shared") &&
		existing.MetadataStamp == modelsdev.CacheStamp() &&
		existing.ProxyVersion == proxyVersion &&
		existing.CodexHome == codexHome && existing.Codex != nil {
		return existing, nil
	}
	// Codex retains its endpoint in memory. Replacing a daemon must bind the
	// same port so retries from already-open native and CCL chats can recover.
	codexAddress := m.CodexListenAddress
	if existing != nil && existing.Codex != nil && existing.CodexHome == codexHome {
		endpoint, err := url.Parse(existing.Codex.BaseURL)
		if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" || endpoint.User != nil || endpoint.Path != "/v1" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, fmt.Errorf("invalid previous Codex proxy endpoint")
		}
		codexAddress = endpoint.Host
	}
	if err := m.stop(ctx); err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(m.Dir, "proxy.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	args := []string{"codex", "serve", "--provider", p.Name, "--instance", id, "--codex-mode", mode}
	if codexAddress != "" {
		args = append(args, "--codex-listen-address", codexAddress)
	}
	child := exec.Command(executable, args...)
	child.Stdout, child.Stderr = log, log
	detach(child)
	if err := child.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = child.Process.Kill()
			<-exited
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cleanupErr := m.stop(cleanupCtx)
			cancel()
			return nil, errors.Join(ctx.Err(), cleanupErr)
		case err := <-exited:
			cleanupErr := m.stop(ctx)
			return nil, errors.Join(fmt.Errorf("proxy exited before readiness (%v); details: %s", err, logPath), cleanupErr)
		case <-ticker.C:
			s, err := m.Read()
			if err != nil {
				continue
			}
			if s != nil && s.ID == id && m.Running(ctx, s) {
				return s, nil
			}
		}
	}
}

// Run owns the entire runtime lifetime. The caller cancels ctx for signals;
// the authenticated local stop endpoint uses the same cleanup path.
func (m *Manager) Run(ctx context.Context, p provider.Provider, id string) (runErr error) {
	mode := m.CodexMode
	if mode == "" {
		mode = "shared"
	}
	if mode != "shared" && mode != "independent" {
		return fmt.Errorf("invalid Codex proxy mode")
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, 250*time.Millisecond)
	release, err := fsutil.AcquireFileLock(lockCtx, filepath.Join(m.Dir, "daemon.lock"))
	cancelLock()
	if err != nil {
		return fmt.Errorf("another proxy is running: %w", err)
	}
	defer release()
	if previous, err := m.Read(); err != nil || previous != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("a previous proxy needs recovery; run ccl codex stop")
	}
	proxy, err := providersession.PrepareProxy(ctx, p)
	if err != nil {
		return err
	}
	defer proxy.Close()
	metadataStamp := modelsdev.CacheStamp()
	codexGateway, err := codex.New(ctx, proxy.Runtime, p, codex.Options{Desktop: mode == "shared", Independent: mode == "independent", ListenAddress: m.CodexListenAddress})
	if err != nil {
		return err
	}
	defer codexGateway.Close()
	codexOverlay, err := codex.Plan(ctx, codexGateway, m.Dir)
	if err != nil {
		return fmt.Errorf("configure Codex: %w", err)
	}
	if codexOverlay == nil {
		return fmt.Errorf("Codex profile not found; open Codex once before starting its proxy")
	}
	journaled := false
	defer func() {
		if !journaled && codexOverlay != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = codexOverlay.Restore(cleanupCtx)
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	token, err := randomID()
	if err != nil {
		return err
	}
	s := &State{ID: id, Provider: p.Name, PID: os.Getpid(), BaseURL: codexGateway.BaseURL(),
		ControlURL: "http://" + listener.Addr().String(), ControlToken: token, StartedAt: time.Now().UTC()}
	s.ProxyVersion = proxyVersion
	s.CodexHome, _ = codex.Home()
	s.Codex = codexOverlay
	s.ConfigHash = providerHash(p)
	s.MetadataStamp = metadataStamp
	s.CodexMode = mode
	// Journal before applying Codex's overlay; fresh daemons never read or
	// write Claude's settings, even if that file is malformed or inaccessible.
	if err := writeJSON(m.statePath(), s); err != nil {
		return err
	}
	defer func() {
		codexGateway.Close()
		proxy.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := restoreState(cleanupCtx, s); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("restore client settings: %w", err))
			return // Keep the journal for the next stop/start to retry.
		}
		runErr = errors.Join(runErr, os.Remove(m.statePath()))
	}()
	journaled = true
	if err := codexOverlay.Apply(ctx); err != nil {
		return fmt.Errorf("apply Codex proxy configuration: %w", err)
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			_, _ = io.WriteString(w, id)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/stop" {
			w.Header().Set("Content-Length", strconv.Itoa(len(id)))
			_, _ = io.WriteString(w, id)
			_ = http.NewResponseController(w).Flush()
			stopOnce.Do(func() { close(stop) })
			return
		}
		http.NotFound(w, r)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer server.Close()
	stopMenuBar := m.startMenuBar(id)
	defer stopMenuBar()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var refresh catalogRefresh
	for {
		select {
		case <-ticker.C:
			if err := refresh.update(m, codexGateway, s, p); err != nil {
				oauthproxy.LogWarnEvent("catalog_refresh_failed", "component", "codex", "error", err)
			}
		case <-ctx.Done():
			return nil
		case <-stop:
			return nil
		case <-proxy.Runtime.Done():
			return fmt.Errorf("provider runtime stopped unexpectedly")
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}
}

func restoreState(ctx context.Context, s *State) error {
	var legacyErr error
	if s.SettingsPath != "" && len(s.Changes) > 0 {
		legacyErr = restoreSettings(ctx, s)
	}
	return errors.Join(legacyErr, s.Codex.Restore(ctx))
}

func providerHash(p provider.Provider) string {
	data, _ := json.Marshal(p)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
