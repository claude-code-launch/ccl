// Package proxymanager owns a standalone provider runtime and a reversible
// Claude Code settings overlay. Its control API only listens on loopback.
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

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type State struct {
	ID           string          `json:"id"`
	Provider     string          `json:"provider"`
	PID          int             `json:"pid"`
	BaseURL      string          `json:"base_url"`
	ControlURL   string          `json:"control_url"`
	ControlToken string          `json:"control_token"`
	SettingsPath string          `json:"settings_path"`
	StartedAt    time.Time       `json:"started_at"`
	ConfigHash   string          `json:"config_hash"`
	EnvBefore    json.RawMessage `json:"env_before,omitempty"`
	Changes      []Change        `json:"changes"`
}

type Manager struct{ Dir string }

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
	if s.ID == "" || s.SettingsPath == "" {
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
	if err := restoreSettings(ctx, current); err != nil {
		return fmt.Errorf("restore Claude settings: %w", err)
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
func (m *Manager) Start(ctx context.Context, executable string, p provider.Provider, settingsPath string) (*State, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	existing, err := m.Read()
	if err != nil {
		return nil, err
	}
	if m.Running(ctx, existing) && existing.ConfigHash == providerHash(p) && existing.SettingsPath == settingsPath {
		return existing, nil
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
	child := exec.Command(executable, "proxy", "serve", "--provider", p.Name, "--instance", id, "--settings-path", settingsPath)
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
func (m *Manager) Run(ctx context.Context, p provider.Provider, settingsPath, id string) (runErr error) {
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
		return fmt.Errorf("a previous proxy needs recovery; run ccl proxy stop")
	}
	proxy, err := claude.PrepareProxy(ctx, p)
	if err != nil {
		return err
	}
	defer proxy.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	token, err := randomID()
	if err != nil {
		return err
	}
	s := &State{ID: id, Provider: p.Name, PID: os.Getpid(), BaseURL: proxy.Runtime.ClaudeBaseURL(),
		ControlURL: "http://" + listener.Addr().String(), ControlToken: token, SettingsPath: settingsPath, StartedAt: time.Now().UTC()}
	s.ConfigHash = providerHash(p)
	if err := applySettings(ctx, s, proxy.Settings, m.statePath()); err != nil {
		// The journal may already exist if the settings write failed.
		if current, _ := m.Read(); current != nil && current.ID == id {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			restoreErr := restoreSettings(cleanupCtx, current)
			cancel()
			if restoreErr == nil {
				_ = os.Remove(m.statePath())
			}
			return errors.Join(err, restoreErr)
		}
		return err
	}
	defer func() {
		proxy.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := restoreSettings(cleanupCtx, s); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("restore Claude settings: %w", err))
			return // Keep the journal for the next stop/start to retry.
		}
		runErr = errors.Join(runErr, os.Remove(m.statePath()))
	}()
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
	select {
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

func providerHash(p provider.Provider) string {
	data, _ := json.Marshal(p)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
