package oauthproxy

import (
	"context"
	"crypto/rand"
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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/google/uuid"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	embeddedCodexOriginator     = "codex_exec"
	fallbackCodexClientVersion  = "0.144.3"
	codexClientVersionEnv       = "CCL_CODEX_CLIENT_VERSION"
	codexClientUserAgentEnv     = "CCL_CODEX_USER_AGENT"
	codexClientDetectionTimeout = 2 * time.Second
	codexOSDetectionTimeout     = time.Second
)

var codexVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

type Runtime struct {
	endpoint    string
	apiKey      string
	service     *cliproxy.Service
	coreManager *coreauth.Manager
	cancel      context.CancelFunc
	done        chan struct{}
	runErr      chan error
	started     chan struct{}
	configPath  string
	runtimeDir  string
	restoreLogs func()
	stopOnce    sync.Once
}

var stdoutMu sync.Mutex

var sdkLogState struct {
	sync.Mutex
	users         int
	previousLevel log.Level
}

type runtimeConfigFile struct {
	Host      string   `yaml:"host"`
	Port      int      `yaml:"port"`
	AuthDir   string   `yaml:"auth-dir"`
	APIKeys   []string `yaml:"api-keys"`
	LogToFile bool     `yaml:"logging-to-file"`
}

type runtimeCodexConfigFile struct {
	Host        string            `yaml:"host"`
	Port        int               `yaml:"port"`
	AuthDir     string            `yaml:"auth-dir"`
	APIKeys     []string          `yaml:"api-keys"`
	LogToFile   bool              `yaml:"logging-to-file"`
	CodexAPIKey []runtimeCodexKey `yaml:"codex-api-key"`
	Payload     runtimePayload    `yaml:"payload"`
}

type runtimeCodexKey struct {
	APIKey  string              `yaml:"api-key"`
	BaseURL string              `yaml:"base-url"`
	Models  []runtimeCodexModel `yaml:"models,omitempty"`
	Headers map[string]string   `yaml:"headers,omitempty"`
}

type runtimeCodexModel struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias,omitempty"`
}

type runtimePayload struct {
	Override []runtimePayloadRule `yaml:"override"`
}

type runtimePayloadRule struct {
	Models []runtimePayloadModel `yaml:"models"`
	Params map[string]any        `yaml:"params"`
}

type runtimePayloadModel struct {
	Name         string `yaml:"name"`
	Protocol     string `yaml:"protocol"`
	FromProtocol string `yaml:"from-protocol"`
}

type providerTokenStore struct {
	backend string
	store   coreauth.Store
}

func newProviderTokenStore(authDir, backend string) *providerTokenStore {
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	return &providerTokenStore{backend: backend, store: store}
}

func (s *providerTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	auths, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), s.backend) {
			filtered = append(filtered, auth)
		}
	}
	return filtered, nil
}

func (s *providerTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth != nil && !strings.EqualFold(strings.TrimSpace(auth.Provider), s.backend) {
		return "", fmt.Errorf("refuse to persist %q credentials in %q OAuth runtime", auth.Provider, s.backend)
	}
	return s.store.Save(ctx, auth)
}

func (s *providerTokenStore) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
}

func Start(parent context.Context, providerName string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	backend, err := BackendProvider(providerName)
	if err != nil {
		return nil, err
	}
	found, err := hasCredential(authDir, backend)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no %s credentials found; run `ccl auth %s` first", backend, providerName)
	}

	port, err := availablePort()
	if err != nil {
		return nil, err
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	configPath, err := writeRuntimeConfig(authDir, port, apiKey)
	if err != nil {
		return nil, err
	}

	cfg := &sdkconfig.Config{
		Host:    "127.0.0.1",
		Port:    port,
		AuthDir: authDir,
	}
	cfg.APIKeys = []string{apiKey}
	cfg.LoggingToFile = false
	store := newProviderTokenStore(authDir, backend)
	return startRuntime(parent, cfg, configPath, apiKey, store, "")
}

// StartCodexAPI starts an embedded CLIProxyAPI runtime configured with a
// Codex-compatible API key endpoint. The SDK supplies the current Codex request
// shape and client headers while ccl continues to expose Anthropic Messages.
func StartCodexAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	if suggestion, invalid := protocol.InvalidCodexV1EndpointSuggestion(endpoint); invalid {
		return nil, fmt.Errorf("invalid Codex endpoint %q: use %q without /v1; ccl requests /models separately", endpoint, suggestion)
	}
	endpoint = codexBaseURL(endpoint)
	if endpoint == "" || strings.TrimSpace(upstreamAPIKey) == "" {
		return nil, fmt.Errorf("Codex Responses runtime requires endpoint and API key")
	}
	codexVersion := detectCodexClientVersion()
	codexUserAgent := buildCodexUserAgent(codexVersion)
	codexIdentity, err := newCodexRequestIdentity()
	if err != nil {
		return nil, err
	}

	runtimeDir, err := os.MkdirTemp("", "ccl-codex-runtime-*")
	if err != nil {
		return nil, fmt.Errorf("create Codex runtime directory: %w", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, err
	}

	port, err := availablePort()
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, err
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, err
	}
	models := make([]runtimeCodexModel, 0)
	for _, model := range strings.Split(modelSpec, ",") {
		model = strings.TrimSpace(model)
		if model != "" {
			models = append(models, runtimeCodexModel{Name: model, Alias: model})
		}
	}
	rawConfig, err := yaml.Marshal(runtimeCodexConfigFile{
		Host:      "127.0.0.1",
		Port:      port,
		AuthDir:   runtimeDir,
		APIKeys:   []string{apiKey},
		LogToFile: false,
		CodexAPIKey: []runtimeCodexKey{{
			APIKey:  upstreamAPIKey,
			BaseURL: endpoint,
			Models:  models,
			Headers: map[string]string{
				"User-Agent":            codexUserAgent,
				"Originator":            embeddedCodexOriginator,
				"Session-Id":            codexIdentity.sessionID,
				"Thread-Id":             codexIdentity.threadID,
				"X-Client-Request-Id":   codexIdentity.sessionID,
				"X-Codex-Window-Id":     codexIdentity.windowID,
				"X-Codex-Turn-Metadata": codexIdentity.turnMetadata,
				"X-Codex-Beta-Features": "remote_compaction_v2",
			},
		}},
		Payload: runtimePayload{Override: []runtimePayloadRule{{
			Models: []runtimePayloadModel{{
				Name:         "*",
				Protocol:     "codex",
				FromProtocol: "responses",
			}},
			Params: map[string]any{
				"prompt_cache_key":                        codexIdentity.sessionID,
				"client_metadata.x-codex-installation-id": codexIdentity.installationID,
				"client_metadata.x-codex-turn-metadata":   codexIdentity.turnMetadata,
				"client_metadata.x-codex-window-id":       codexIdentity.windowID,
				"client_metadata.session_id":              codexIdentity.sessionID,
				"client_metadata.thread_id":               codexIdentity.threadID,
				"client_metadata.turn_id":                 codexIdentity.turnID,
			},
		}}},
	})
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("encode Codex runtime config: %w", err)
	}
	cfg, err := sdkconfig.ParseConfigBytes(rawConfig)
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("parse Codex runtime config: %w", err)
	}
	configPath, err := writeRuntimeConfigData(rawConfig)
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, err
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(runtimeDir)
	return startRuntime(parent, cfg, configPath, apiKey, store, runtimeDir)
}

type codexRequestIdentity struct {
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
	turnMetadata   string
}

func newCodexRequestIdentity() (codexRequestIdentity, error) {
	installationID := uuid.NewString()
	sessionID := uuid.NewString()
	threadID := sessionID
	turnID := uuid.NewString()
	windowID := sessionID + ":0"
	metadata, err := json.Marshal(map[string]any{
		"installation_id":         installationID,
		"session_id":              sessionID,
		"thread_id":               threadID,
		"turn_id":                 turnID,
		"window_id":               windowID,
		"request_kind":            "turn",
		"thread_source":           "user",
		"sandbox":                 "seatbelt",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	})
	if err != nil {
		return codexRequestIdentity{}, fmt.Errorf("encode Codex turn metadata: %w", err)
	}
	return codexRequestIdentity{
		installationID: installationID,
		sessionID:      sessionID,
		threadID:       threadID,
		turnID:         turnID,
		windowID:       windowID,
		turnMetadata:   string(metadata),
	}, nil
}

func detectCodexClientVersion() string {
	if version := parseCodexClientVersion(os.Getenv(codexClientVersionEnv)); version != "" {
		return version
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexClientDetectionTimeout)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "codex", "--version").Output(); err == nil {
		if version := parseCodexClientVersion(string(output)); version != "" {
			return version
		}
	}
	return fallbackCodexClientVersion
}

func parseCodexClientVersion(value string) string {
	return codexVersionPattern.FindString(strings.TrimSpace(value))
}

func buildCodexUserAgent(version string) string {
	if override := strings.TrimSpace(os.Getenv(codexClientUserAgentEnv)); override != "" {
		return override
	}
	osType, osVersion := codexOSInfo()
	userAgent := fmt.Sprintf(
		"%s/%s (%s %s; %s)",
		embeddedCodexOriginator,
		version,
		osType,
		osVersion,
		runtime.GOARCH,
	)
	if terminal := terminalUserAgentToken(); terminal != "" {
		userAgent += " " + terminal
	}
	return fmt.Sprintf("%s (%s; %s)", userAgent, embeddedCodexOriginator, version)
}

func codexOSInfo() (string, string) {
	switch runtime.GOOS {
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), codexOSDetectionTimeout)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "sw_vers", "-productVersion").Output(); err == nil {
			if version := strings.TrimSpace(string(output)); version != "" {
				return "Mac OS", version
			}
		}
		return "Mac OS", "unknown"
	case "windows":
		return "Windows", "unknown"
	default:
		return "Linux", "unknown"
	}
}

func terminalUserAgentToken() string {
	program := sanitizeUserAgentToken(os.Getenv("TERM_PROGRAM"))
	if program == "" {
		return sanitizeUserAgentToken(os.Getenv("TERM"))
	}
	if version := sanitizeUserAgentToken(os.Getenv("TERM_PROGRAM_VERSION")); version != "" {
		return program + "/" + version
	}
	return program
}

func sanitizeUserAgentToken(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, char := range value {
		if char >= '!' && char <= '~' && char != '(' && char != ')' {
			b.WriteRune(char)
		} else if b.Len() > 0 {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func startRuntime(parent context.Context, cfg *sdkconfig.Config, configPath, apiKey string, store coreauth.Store, runtimeDir string) (*Runtime, error) {
	coreManager := coreauth.NewManager(store, nil, nil)
	restoreLogs := silenceSDKLogs()
	started := make(chan struct{})
	service, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithAuthManager(sdkauth.NewManager(store)).
		WithCoreAuthManager(coreManager).
		WithWatcherFactory(func(string, string, func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			return &cliproxy.WatcherWrapper{}, nil
		}).
		WithHooks(cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) { close(started) }}).
		Build()
	if err != nil {
		restoreLogs()
		_ = os.Remove(configPath)
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("build embedded CLIProxyAPI: %w", err)
	}

	runCtx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		endpoint:    "http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/v1",
		apiKey:      apiKey,
		service:     service,
		coreManager: coreManager,
		cancel:      cancel,
		done:        make(chan struct{}),
		runErr:      make(chan error, 1),
		started:     started,
		configPath:  configPath,
		runtimeDir:  runtimeDir,
		restoreLogs: restoreLogs,
	}
	restoreStdout := silenceStdout()
	go func() {
		runtime.runErr <- service.Run(runCtx)
		close(runtime.done)
	}()

	err = runtime.waitReady(runCtx)
	restoreStdout()
	if err != nil {
		runtime.Stop()
		return nil, err
	}
	return runtime, nil
}

func silenceSDKLogs() func() {
	sdkLogState.Lock()
	if sdkLogState.users == 0 {
		sdkLogState.previousLevel = log.GetLevel()
		log.SetOutput(io.Discard)
		log.SetLevel(log.PanicLevel)
	}
	sdkLogState.users++
	sdkLogState.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			sdkLogState.Lock()
			defer sdkLogState.Unlock()
			sdkLogState.users--
			if sdkLogState.users == 0 {
				// CLIProxyAPI refresh workers can finish after Service.Shutdown and
				// emit late warnings. ccl does not otherwise use logrus, so keep its
				// output isolated for the remainder of this process.
				log.SetOutput(io.Discard)
				log.SetLevel(sdkLogState.previousLevel)
			}
		})
	}
}

func silenceStdout() func() {
	stdoutMu.Lock()
	original := os.Stdout
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		stdoutMu.Unlock()
		return func() {}
	}
	os.Stdout = sink
	var once sync.Once
	return func() {
		once.Do(func() {
			os.Stdout = original
			_ = sink.Close()
			stdoutMu.Unlock()
		})
	}
}

func hasCredential(authDir, backend string) (bool, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return false, fmt.Errorf("read auth directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(authDir, entry.Name()))
		if err != nil {
			return false, fmt.Errorf("read credential %s: %w", entry.Name(), err)
		}
		var metadata struct {
			Type     string `json:"type"`
			Disabled bool   `json:"disabled"`
		}
		if json.Unmarshal(data, &metadata) == nil &&
			!metadata.Disabled && strings.EqualFold(strings.TrimSpace(metadata.Type), backend) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runtime) Endpoint() string { return r.endpoint }

func (r *Runtime) APIKey() string { return r.apiKey }

func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		stopped := false
		select {
		case <-r.done:
			stopped = true
		case <-time.After(5 * time.Second):
		}
		// Service.Run performs its own deferred Shutdown after the run context is
		// canceled. Calling Shutdown concurrently with its final initialization
		// races inside CLIProxyAPI, so force it only if Run failed to stop in time.
		if !stopped && r.service != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = r.service.Shutdown(ctx)
			cancel()
			select {
			case <-r.done:
			case <-time.After(5 * time.Second):
			}
		}
		if r.coreManager != nil {
			registry := cliproxy.GlobalModelRegistry()
			for _, auth := range r.coreManager.List() {
				if auth != nil && auth.ID != "" {
					registry.UnregisterClient(auth.ID)
				}
			}
		}
		_ = os.Remove(r.configPath)
		if r.runtimeDir != "" {
			_ = os.RemoveAll(r.runtimeDir)
		}
		if r.restoreLogs != nil {
			r.restoreLogs()
		}
	})
}

func (r *Runtime) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	healthURL := r.endpoint[:len(r.endpoint)-len("/v1")] + "/healthz"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.done:
			err := <-r.runErr
			if err == nil || errors.Is(err, context.Canceled) {
				return fmt.Errorf("embedded CLIProxyAPI stopped before becoming ready")
			}
			return fmt.Errorf("start embedded CLIProxyAPI: %w", err)
		case <-deadline.C:
			return fmt.Errorf("embedded CLIProxyAPI did not become ready within 10 seconds")
		case <-r.started:
			r.started = nil
		case <-ticker.C:
			if r.started != nil {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func availablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve local proxy port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release local proxy port: %w", err)
	}
	return port, nil
}

func sessionAPIKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate local proxy key: %w", err)
	}
	return "ccl-" + hex.EncodeToString(raw), nil
}

func writeRuntimeConfig(authDir string, port int, apiKey string) (string, error) {
	data, err := yaml.Marshal(runtimeConfigFile{
		Host:      "127.0.0.1",
		Port:      port,
		AuthDir:   authDir,
		APIKeys:   []string{apiKey},
		LogToFile: false,
	})
	if err != nil {
		return "", fmt.Errorf("encode embedded proxy config: %w", err)
	}
	return writeRuntimeConfigData(data)
}

func writeRuntimeConfigData(data []byte) (string, error) {
	file, err := os.CreateTemp("", "ccl-cliproxy-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create embedded proxy config: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func codexBaseURL(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/responses", "/chat/completions", "/models"} {
		if strings.HasSuffix(parsed.Path, suffix) {
			parsed.Path = strings.TrimSuffix(parsed.Path, suffix)
			break
		}
	}
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}
