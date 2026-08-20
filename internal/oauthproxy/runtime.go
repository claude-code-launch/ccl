package oauthproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

const (
	// runtimeStopTimeout bounds each teardown wait in Runtime.Stop.
	runtimeStopTimeout  = 5 * time.Second
	runtimeLoopbackHost = "127.0.0.1"
)

type Runtime struct {
	endpoint   string
	apiKey     string
	httpServer *http.Server
	listAuths  func() []*AuthInfo
	cleanup    []func()
	cancel     context.CancelFunc
	done       chan struct{}
	runErr     chan error
	started    chan struct{}
	models     []string
	modelNames map[string]string
	ownsLog    bool
	stopOnce   sync.Once
	// usage accumulates per-model token totals for this runtime. It is never nil:
	// StartProvider always installs one, even when the backend cannot report
	// usage, so callers do not need a nil check.
	usage *UsageTracker
}

// Usage returns the token usage accumulated by this runtime so far. Safe to
// call at any point in the runtime's lifetime, including after Stop.
func (r *Runtime) Usage() *UsageTracker {
	if r == nil {
		return nil
	}
	return r.usage
}

// Models returns the authoritative upstream catalog captured when the runtime
// started. It avoids treating compatibility-layer built-ins as provider models.
func (r *Runtime) Models() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.models...)
}

// ModelDisplayNames returns the provider catalog's human-facing labels keyed
// by technical model ID. Direct adapters may expose these labels as UI aliases
// when they also resolve each alias back to the ID before the upstream request.
func (r *Runtime) ModelDisplayNames() map[string]string {
	if r == nil || len(r.modelNames) == 0 {
		return nil
	}
	names := make(map[string]string, len(r.modelNames))
	for id, name := range r.modelNames {
		names[id] = name
	}
	return names
}

type UpstreamProtocol string

const (
	ProtocolOpenAIChat      UpstreamProtocol = "openai_chat"
	ProtocolOpenAIResponses UpstreamProtocol = "openai_responses"
	ProtocolCommandCode     UpstreamProtocol = "commandcode"
)

type StartOptions struct {
	Protocol      UpstreamProtocol
	Endpoint      string
	APIKey        string
	ModelSpec     string
	OAuthProvider string
	// OAuthAccountCredential optionally restricts the runtime to a single
	// credential file (basename under the OAuth auth dir) for this backend.
	OAuthAccountCredential string
	// ModelProtocols maps a lowercase model ID to its upstream protocol for the
	// mixed-protocol models.dev gateway. When non-empty it takes precedence over
	// the single Protocol value and starts the mixed-protocol router.
	ModelProtocols map[string]string
}

type runtimeModelRoute struct {
	Name  string
	Alias string
}

// StartProvider starts a loopback Anthropic Messages adapter. Every protocol
// family is served by a CCL-owned data plane (Codex Responses, OpenAI Chat, or
// the native Anthropic passthrough).
func StartProvider(parent context.Context, options StartOptions) (*Runtime, error) {
	_, ownsLog, logErr := EnsureSessionLog("runtime")
	if logErr != nil {
		return nil, fmt.Errorf("open provider runtime log: %w", logErr)
	}
	runtime, err := startProvider(parent, options)
	if err != nil {
		// Successful starts are logged by each start function; failures were only
		// visible to the caller, which made a session that never launched look
		// like an empty log.
		LogErrorf("runtime start failed oauth=%q protocol=%q endpoint=%q credential=%q error=%v",
			options.OAuthProvider, options.Protocol, SafeLogEndpoint(options.Endpoint),
			options.OAuthAccountCredential, err)
		if ownsLog {
			CloseLog()
		}
		return nil, err
	}
	runtime.ownsLog = ownsLog
	return runtime, nil
}

func startProvider(parent context.Context, options StartOptions) (*Runtime, error) {
	if strings.TrimSpace(options.OAuthProvider) != "" {
		return StartOAuth(parent, options.OAuthProvider, options.ModelSpec, options.OAuthAccountCredential)
	}
	if len(options.ModelProtocols) > 0 {
		return StartMixedProtocolAPIKeyRuntime(parent, options.Endpoint, options.APIKey, options.ModelSpec, options.ModelProtocols)
	}
	switch options.Protocol {
	case ProtocolOpenAIChat:
		return StartOpenAIChatAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
	case ProtocolOpenAIResponses:
		return StartOpenAIResponsesAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
	case ProtocolCommandCode:
		return StartCommandCodeAPI(parent, options.Endpoint, options.APIKey, options.ModelSpec)
	default:
		return nil, fmt.Errorf("unsupported embedded proxy protocol %q", options.Protocol)
	}
}

func StartOAuth(parent context.Context, providerName, modelSpec, credentialFile string) (*Runtime, error) {
	credentialFile = strings.TrimSpace(credentialFile)
	if credentialFile == "" {
		return nil, fmt.Errorf("subscription provider %s is not bound to a credential file; authenticate the account again with `ccl oauth %s [alias]`", providerName, providerName)
	}
	if parent == nil {
		parent = context.Background()
	}
	backend, err := BackendProvider(providerName)
	if err != nil {
		return nil, err
	}
	if backend == ProviderKiro {
		return startKiroOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderCopilot {
		return startCopilotOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderQoder {
		return startQoderOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderCodex {
		return startCodexOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderWorkBuddy {
		return startWorkBuddyOAuth(parent, modelSpec, credentialFile)
	}
	if backend == backendXAI {
		return startXaiOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderKimi {
		return startKimiOAuth(parent, modelSpec, credentialFile)
	}
	if backend == ProviderCommandCode {
		return startCommandCodeOAuth(parent, modelSpec, credentialFile)
	}
	if backend == backendAntigravity {
		return startAntigravityOAuth(parent, modelSpec, credentialFile)
	}
	return nil, fmt.Errorf("unsupported subscription provider %q", providerName)
}

// StartOpenAIChatAPI starts CCL's self-owned Chat Completions adapter against
// an OpenAI-compatible API-key gateway. Request conversion, SSE conversion,
// error mapping, and usage accounting are all owned by CCL and cannot change
// with a CLIProxyAPI upgrade.
func StartOpenAIChatAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	proxyRuntime, err := startOpenAIChatRuntime(parent, endpoint, upstreamAPIKey, modelSpec)
	if err != nil {
		return nil, err
	}
	LogInfof("runtime start openai_chat endpoint=%q local_endpoint=%q model_count=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.Endpoint()), len(routes))
	return proxyRuntime, nil
}

// StartOpenAIResponsesAPI starts CCL's Codex Responses adapter against an API
// key gateway. Request conversion, Codex identity, SSE conversion, errors, and
// usage accounting are all owned by CCL and cannot change with a CPA upgrade.
func StartOpenAIResponsesAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
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

	proxyRuntime, err := startCodexResponsesAPI(parent, endpoint, upstreamAPIKey, modelSpec)
	if err != nil {
		return nil, err
	}
	LogInfof("runtime start codex_responses auth=api_key endpoint=%q local_endpoint=%q model_count=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.Endpoint()), len(routes))
	return proxyRuntime, nil
}

// StartCommandCodeAPI starts CCL's self-owned Command Code data plane against a
// Command Code API key. Conversion, NDJSON/SSE handling, identity headers, the
// fingerprint/lifecycle handshake, error mapping, and usage accounting are all
// owned by CCL and cannot change with a CLIProxyAPI upgrade. modelSpec is
// accepted for StartOptions symmetry only: the runtime serves the authoritative
// 26-model catalog and never rewrites requested model IDs.
func StartCommandCodeAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	proxyRuntime, err := startCommandCodeRuntime(parent, endpoint, upstreamAPIKey)
	if err != nil {
		return nil, err
	}
	LogInfof("runtime start commandcode endpoint=%q local_endpoint=%q model_count=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.Endpoint()), len(proxyRuntime.Models()))
	return proxyRuntime, nil
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

// runtimeModelAliases returns the distinct client-facing aliases of a model spec,
// keeping the order in which they were configured.
func runtimeModelAliases(modelSpec string) []string {
	routes := runtimeModelRoutes(modelSpec)
	aliases := make([]string, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		alias := strings.TrimSpace(route.Alias)
		key := strings.ToLower(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, alias)
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

func (r *Runtime) Endpoint() string { return r.endpoint }

// ListAuths returns the credentials currently loaded in this runtime, already
// filtered to the OAuth backend and selected account.
func (r *Runtime) ListAuths() []*AuthInfo {
	if r == nil {
		return nil
	}
	if r.listAuths != nil {
		return r.listAuths()
	}
	return nil
}

// ClaudeBaseURL is the origin Claude Code uses before appending /v1/messages.
// Endpoint includes /v1 because ccl's model and diagnostics clients expect an
// OpenAI API root.
func (r *Runtime) ClaudeBaseURL() string {
	return strings.TrimSuffix(r.endpoint, "/v1")
}

func (r *Runtime) APIKey() string { return r.apiKey }

// Stop cancels the run context, waits for Serve to exit on its own, and only
// force-calls http.Server.Shutdown if that wait times out.
func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		stopStarted := time.Now()
		if r.cancel != nil {
			r.cancel()
		}
		stopped := waitClosed(r.done, runtimeStopTimeout)
		defer func() {
			LogInfof("runtime stop endpoint=%q clean_exit=%t duration=%s",
				SafeLogEndpoint(r.endpoint), stopped, time.Since(stopStarted).Round(time.Millisecond))
			if r.ownsLog {
				CloseLog()
			}
		}()
		// Force Shutdown only when Run did not exit cleanly in time.
		if !stopped && r.httpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), runtimeStopTimeout)
			_ = r.httpServer.Shutdown(ctx)
			cancel()
			waitClosed(r.done, runtimeStopTimeout)
		}
		for _, cleanup := range r.cleanup {
			if cleanup != nil {
				cleanup()
			}
		}
	})
}

// waitClosed reports whether done closed within timeout. Unlike time.After it
// releases the timer as soon as the wait finishes, which matters because Stop can
// be called for every launched runtime.
func waitClosed(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func sessionAPIKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate local proxy key: %w", err)
	}
	return "ccl-" + hex.EncodeToString(raw), nil
}

// normalizeOpenAIBaseURL strips trailing generation paths (/responses,
// /chat/completions, /models) so the runtime receives an API root.
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
