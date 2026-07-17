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

	"github.com/claude-code-launch/ccl/internal/modelrouting"
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
	embeddedCodexOriginator     = "codex_cli_rs"
	fallbackCodexClientVersion  = "0.144.4"
	codexClientVersionEnv       = "CCL_CODEX_CLIENT_VERSION"
	codexClientUserAgentEnv     = "CCL_CODEX_USER_AGENT"
	codexClientDetectionTimeout = 2 * time.Second
	codexOSDetectionTimeout     = time.Second
)

var codexVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

type Runtime struct {
	endpoint        string
	apiKey          string
	service         *cliproxy.Service
	coreManager     *coreauth.Manager
	responsesCompat *responsesCompatibilityProxy
	cancel          context.CancelFunc
	done            chan struct{}
	runErr          chan error
	started         chan struct{}
	configPath      string
	runtimeDir      string
	restoreLogs     func()
	stopOnce        sync.Once
}

type UpstreamProtocol string

const (
	ProtocolOpenAIChat      UpstreamProtocol = "openai_chat"
	ProtocolOpenAIResponses UpstreamProtocol = "openai_responses"
)

type StartOptions struct {
	Protocol      UpstreamProtocol
	Endpoint      string
	APIKey        string
	ModelSpec     string
	OAuthProvider string
}

type runtimeModelRoute struct {
	Name  string
	Alias string
}

// stdoutState reference-counts temporary redirection of os.Stdout to
// os.DevNull while embedded CLIProxyAPI services become ready. Nested
// startRuntime calls must share one sink instead of stacking file
// descriptors or restoring a mid-stack original too early.
var stdoutState struct {
	sync.Mutex
	users    int
	original *os.File
	sink     *os.File
}

var sdkLogState struct {
	sync.Mutex
	users         int
	previousLevel log.Level
}

type runtimeConfigFile struct {
	Host                   string                              `yaml:"host"`
	Port                   int                                 `yaml:"port"`
	AuthDir                string                              `yaml:"auth-dir"`
	APIKeys                []string                            `yaml:"api-keys"`
	LogToFile              bool                                `yaml:"logging-to-file"`
	DisableImageGeneration string                              `yaml:"disable-image-generation,omitempty"`
	OAuthModelAlias        map[string][]runtimeOAuthModelAlias `yaml:"oauth-model-alias,omitempty"`
}

type runtimeCodexConfigFile struct {
	Host                   string                              `yaml:"host"`
	Port                   int                                 `yaml:"port"`
	AuthDir                string                              `yaml:"auth-dir"`
	APIKeys                []string                            `yaml:"api-keys"`
	LogToFile              bool                                `yaml:"logging-to-file"`
	DisableImageGeneration string                              `yaml:"disable-image-generation,omitempty"`
	CodexAPIKey            []runtimeCodexKey                   `yaml:"codex-api-key"`
	OAuthModelAlias        map[string][]runtimeOAuthModelAlias `yaml:"oauth-model-alias,omitempty"`
}

type runtimeOpenAIConfigFile struct {
	Host                   string                       `yaml:"host"`
	Port                   int                          `yaml:"port"`
	AuthDir                string                       `yaml:"auth-dir"`
	APIKeys                []string                     `yaml:"api-keys"`
	LogToFile              bool                         `yaml:"logging-to-file"`
	DisableImageGeneration string                       `yaml:"disable-image-generation,omitempty"`
	OpenAICompatibility    []runtimeOpenAICompatibility `yaml:"openai-compatibility"`
}

type runtimeOAuthModelAlias struct {
	Name         string `yaml:"name"`
	Alias        string `yaml:"alias"`
	Fork         bool   `yaml:"fork,omitempty"`
	ForceMapping bool   `yaml:"force-mapping,omitempty"`
}

type runtimeOpenAICompatibility struct {
	Name          string                          `yaml:"name"`
	BaseURL       string                          `yaml:"base-url"`
	APIKeyEntries []runtimeOpenAICompatibilityKey `yaml:"api-key-entries"`
	Models        []runtimeOpenAIModel            `yaml:"models"`
}

type runtimeOpenAICompatibilityKey struct {
	APIKey string `yaml:"api-key"`
}

type runtimeOpenAIModel struct {
	Name         string `yaml:"name"`
	Alias        string `yaml:"alias"`
	ForceMapping bool   `yaml:"force-mapping,omitempty"`
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
	return StartOAuth(parent, providerName, "")
}

// StartProvider starts the embedded CLIProxyAPI adapter for every OpenAI-family
// provider. Claude Code connects directly to the runtime's /v1/messages route.
//
// openai_responses is split:
//   - dedicated Codex bases (…/codex) → StartCodexAPI with Codex client identity
//   - plain Responses gateways → StartOpenAIResponsesAPI without Codex headers/body
//
// Invalid Codex paths such as …/codex/v1 are rejected before routing so they
// cannot fall through to the plain Responses path and hit …/codex/v1/responses.
func StartProvider(parent context.Context, options StartOptions) (*Runtime, error) {
	if strings.TrimSpace(options.OAuthProvider) != "" {
		return StartOAuth(parent, options.OAuthProvider, options.ModelSpec)
	}
	switch options.Protocol {
	case ProtocolOpenAIChat:
		return StartOpenAIChatAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
	case ProtocolOpenAIResponses:
		if suggestion, invalid := protocol.InvalidCodexV1EndpointSuggestion(options.Endpoint); invalid {
			return nil, fmt.Errorf("invalid Codex endpoint %q: use %q without /v1; ccl requests /models separately", options.Endpoint, suggestion)
		}
		if protocol.IsCodexBaseEndpoint(options.Endpoint) {
			return StartCodexAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
		}
		return StartOpenAIResponsesAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
	default:
		return nil, fmt.Errorf("unsupported embedded proxy protocol %q", options.Protocol)
	}
}

func StartOAuth(parent context.Context, providerName, modelSpec string) (*Runtime, error) {
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
	aliases := oauthModelAliases(modelSpec)
	rawConfig, err := yaml.Marshal(runtimeConfigFile{
		Host:                   "127.0.0.1",
		Port:                   port,
		AuthDir:                authDir,
		APIKeys:                []string{apiKey},
		LogToFile:              false,
		DisableImageGeneration: "passthrough",
		OAuthModelAlias: map[string][]runtimeOAuthModelAlias{
			backend: aliases,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode embedded OAuth proxy config: %w", err)
	}
	cfg, err := sdkconfig.ParseConfigBytes(rawConfig)
	if err != nil {
		return nil, fmt.Errorf("parse embedded OAuth proxy config: %w", err)
	}
	configPath, err := writeRuntimeConfigData(rawConfig)
	if err != nil {
		return nil, err
	}
	store := newProviderTokenStore(authDir, backend)
	return startRuntime(parent, cfg, configPath, apiKey, store, "")
}

// StartOpenAIChatAPI starts CLIProxyAPI with an OpenAI-compatible Chat
// Completions upstream. CLIProxyAPI owns both request and response translation.
func StartOpenAIChatAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	endpoint = normalizeOpenAIBaseURL(endpoint)
	if endpoint == "" || strings.TrimSpace(upstreamAPIKey) == "" {
		return nil, fmt.Errorf("OpenAI Chat runtime requires endpoint and API key")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}

	runtimeDir, port, apiKey, err := prepareAPIKeyRuntime()
	if err != nil {
		return nil, err
	}
	models := make([]runtimeOpenAIModel, 0, len(routes))
	for _, route := range routes {
		models = append(models, runtimeOpenAIModel{
			Name:         route.Name,
			Alias:        route.Alias,
			ForceMapping: true,
		})
	}
	rawConfig, err := yaml.Marshal(runtimeOpenAIConfigFile{
		Host:                   "127.0.0.1",
		Port:                   port,
		AuthDir:                runtimeDir,
		APIKeys:                []string{apiKey},
		LogToFile:              false,
		DisableImageGeneration: "passthrough",
		OpenAICompatibility: []runtimeOpenAICompatibility{{
			Name:    "ccl-openai-chat",
			BaseURL: endpoint,
			APIKeyEntries: []runtimeOpenAICompatibilityKey{{
				APIKey: upstreamAPIKey,
			}},
			Models: models,
		}},
	})
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("encode OpenAI Chat runtime config: %w", err)
	}
	return startAPIKeyRuntime(parent, rawConfig, apiKey, runtimeDir)
}

// StartOpenAIResponsesAPI starts an embedded CLIProxyAPI runtime against a plain
// OpenAI Responses upstream (not a dedicated Codex base).
//
// CLIProxyAPI only exposes a Responses upstream executor through codex-api-key,
// so the config still uses that slot. Unlike StartCodexAPI, no Codex Originator /
// User-Agent / client_metadata / session headers are injected — plain gateways
// often reject those as unsupported parameters.
func StartOpenAIResponsesAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	return startResponsesRuntime(parent, endpoint, upstreamAPIKey, modelSpec, false)
}

// StartCodexAPI starts an embedded CLIProxyAPI runtime for a dedicated Codex
// Responses endpoint (…/codex). It injects Codex client identity headers and
// body metadata required by Codex-compatible upstreams.
func StartCodexAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	if suggestion, invalid := protocol.InvalidCodexV1EndpointSuggestion(endpoint); invalid {
		return nil, fmt.Errorf("invalid Codex endpoint %q: use %q without /v1; ccl requests /models separately", endpoint, suggestion)
	}
	return startResponsesRuntime(parent, endpoint, upstreamAPIKey, modelSpec, true)
}

func startResponsesRuntime(parent context.Context, endpoint, upstreamAPIKey, modelSpec string, codexIdentity bool) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	if endpoint == "" || strings.TrimSpace(upstreamAPIKey) == "" {
		return nil, fmt.Errorf("OpenAI Responses runtime requires endpoint and API key")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Responses runtime requires at least one model")
	}

	var identity *codexRequestIdentity
	var headers map[string]string
	if codexIdentity {
		codexVersion := detectCodexClientVersion()
		codexUserAgent := buildCodexUserAgent(codexVersion)
		id, err := newCodexRequestIdentity()
		if err != nil {
			return nil, err
		}
		identity = &id
		headers = map[string]string{
			"User-Agent":            codexUserAgent,
			"Originator":            embeddedCodexOriginator,
			"X-Codex-Beta-Features": "remote_compaction_v2",
		}
	}

	runtimeDir, port, apiKey, err := prepareAPIKeyRuntime()
	if err != nil {
		return nil, err
	}
	compat, err := startResponsesCompatibilityProxy(endpoint, identity)
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, err
	}
	models := make([]runtimeCodexModel, 0, len(routes))
	for _, route := range routes {
		models = append(models, runtimeCodexModel{Name: route.Name, Alias: route.Alias})
	}
	rawConfig, err := yaml.Marshal(runtimeCodexConfigFile{
		Host:                   "127.0.0.1",
		Port:                   port,
		AuthDir:                runtimeDir,
		APIKeys:                []string{apiKey},
		LogToFile:              false,
		DisableImageGeneration: "passthrough",
		CodexAPIKey: []runtimeCodexKey{{
			APIKey:  upstreamAPIKey,
			BaseURL: compat.endpoint,
			Models:  models,
			Headers: headers,
		}},
	})
	if err != nil {
		compat.Stop()
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("encode OpenAI Responses runtime config: %w", err)
	}
	proxyRuntime, err := startAPIKeyRuntime(parent, rawConfig, apiKey, runtimeDir)
	if err != nil {
		compat.Stop()
		return nil, err
	}
	proxyRuntime.responsesCompat = compat
	return proxyRuntime, nil
}

func prepareAPIKeyRuntime() (runtimeDir string, port int, apiKey string, err error) {
	runtimeDir, err = os.MkdirTemp("", "ccl-cliproxy-runtime-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("create CLIProxyAPI runtime directory: %w", err)
	}
	if err = os.Chmod(runtimeDir, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDir)
		return "", 0, "", err
	}
	port, err = availablePort()
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return "", 0, "", err
	}
	apiKey, err = sessionAPIKey()
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return "", 0, "", err
	}
	return runtimeDir, port, apiKey, nil
}

func startAPIKeyRuntime(parent context.Context, rawConfig []byte, apiKey, runtimeDir string) (*Runtime, error) {
	cfg, err := sdkconfig.ParseConfigBytes(rawConfig)
	if err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("parse embedded CLIProxyAPI config: %w", err)
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

func runtimeModelRoutes(modelSpec string) []runtimeModelRoute {
	routes := make([]runtimeModelRoute, 0)
	seen := make(map[string]bool)
	add := func(name, alias string) {
		name = strings.TrimSpace(name)
		alias = strings.TrimSpace(alias)
		if name == "" || alias == "" {
			return
		}
		key := strings.ToLower(name) + "\x00" + strings.ToLower(alias)
		if seen[key] {
			return
		}
		seen[key] = true
		routes = append(routes, runtimeModelRoute{Name: name, Alias: alias})
	}
	for _, configured := range modelrouting.SplitCSV(modelSpec) {
		upstream := stripContextModelSuffix(configured)
		add(upstream, upstream)
		if !strings.EqualFold(upstream, configured) {
			add(upstream, configured)
		}
	}
	return routes
}

func oauthModelAliases(modelSpec string) []runtimeOAuthModelAlias {
	aliases := make([]runtimeOAuthModelAlias, 0)
	for _, route := range runtimeModelRoutes(modelSpec) {
		if strings.EqualFold(route.Name, route.Alias) {
			continue
		}
		aliases = append(aliases, runtimeOAuthModelAlias{
			Name:         route.Name,
			Alias:        route.Alias,
			Fork:         true,
			ForceMapping: true,
		})
	}
	return aliases
}

func stripContextModelSuffix(model string) string {
	model = strings.TrimSpace(model)
	for strings.HasSuffix(strings.ToLower(model), "[1m]") {
		model = strings.TrimSpace(model[:len(model)-len("[1m]")])
	}
	return model
}

type codexRequestIdentity struct {
	installationID string
	sessionID      string
	turnID         string
}

func newCodexRequestIdentity() (codexRequestIdentity, error) {
	installationID := uuid.NewString()
	sessionID := uuid.NewString()
	turnID := uuid.NewString()
	return codexRequestIdentity{
		installationID: installationID,
		sessionID:      sessionID,
		turnID:         turnID,
	}, nil
}

func detectCodexClientVersion() string {
	if version := parseCodexClientVersion(os.Getenv(codexClientVersionEnv)); version != "" {
		return version
	}
	version := fallbackCodexClientVersion
	ctx, cancel := context.WithTimeout(context.Background(), codexClientDetectionTimeout)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "codex", "--version").Output(); err == nil {
		if detected := parseCodexClientVersion(string(output)); newerCodexClientVersion(detected, version) {
			version = detected
		}
	}
	return version
}

func parseCodexClientVersion(value string) string {
	return codexVersionPattern.FindString(strings.TrimSpace(value))
}

func newerCodexClientVersion(candidate, baseline string) bool {
	parse := func(version string) [3]int {
		version = strings.SplitN(version, "-", 2)[0]
		version = strings.SplitN(version, "+", 2)[0]
		parts := strings.Split(version, ".")
		var parsed [3]int
		for i := 0; i < len(parts) && i < len(parsed); i++ {
			parsed[i], _ = strconv.Atoi(parts[i])
		}
		return parsed
	}
	candidateParts := parse(candidate)
	baselineParts := parse(baseline)
	for i := range candidateParts {
		if candidateParts[i] != baselineParts[i] {
			return candidateParts[i] > baselineParts[i]
		}
	}
	return false
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
	return userAgent
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
		if terminal := sanitizeUserAgentToken(os.Getenv("TERM")); terminal != "" {
			return terminal
		}
		return "unknown"
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
	stdoutState.Lock()
	if stdoutState.users == 0 {
		sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			stdoutState.Unlock()
			return func() {}
		}
		stdoutState.original = os.Stdout
		stdoutState.sink = sink
		os.Stdout = sink
	}
	stdoutState.users++
	stdoutState.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			stdoutState.Lock()
			defer stdoutState.Unlock()
			if stdoutState.users == 0 {
				return
			}
			stdoutState.users--
			if stdoutState.users > 0 {
				return
			}
			os.Stdout = stdoutState.original
			if stdoutState.sink != nil {
				_ = stdoutState.sink.Close()
			}
			stdoutState.original = nil
			stdoutState.sink = nil
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

// ClaudeBaseURL is the origin Claude Code uses before appending /v1/messages.
// Endpoint includes /v1 because ccl's model and diagnostics clients expect an
// OpenAI API root.
func (r *Runtime) ClaudeBaseURL() string {
	return strings.TrimSuffix(r.endpoint, "/v1")
}

func (r *Runtime) APIKey() string { return r.apiKey }

// Stop tears down the embedded CLIProxyAPI service.
//
// Teardown order is part of the CLIProxyAPI compatibility boundary (see
// package doc): cancel the run context, wait for Service.Run to exit on its
// own, and only force Service.Shutdown if that wait times out. Concurrent
// Shutdown during Run's deferred cleanup races inside the SDK.
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
		// Force Shutdown only when Run did not exit cleanly in time.
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
		if r.responsesCompat != nil {
			r.responsesCompat.Stop()
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

// normalizeOpenAIBaseURL strips trailing generation paths (/responses,
// /chat/completions, /models) so CLIProxyAPI config receives an API root.
// Used for both plain OpenAI Chat/Responses and dedicated Codex bases.
func normalizeOpenAIBaseURL(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/responses", "/chat/completions", "/models"} {
		if rest, ok := strings.CutSuffix(parsed.Path, suffix); ok {
			parsed.Path = rest
			break
		}
	}
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}
