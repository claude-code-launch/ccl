// Package codex connects Codex clients to CCL providers without moving native
// Codex credentials. The desktop picker and routing allow-list are
// deliberately separate: only names owned by CCL can reach a CCL runtime.
package codex

import (
	"bytes"
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
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/klauspost/compress/zstd"
)

const maxBodyBytes = 32 << 20

type Options struct {
	// DeferredStart lets the daemon finish its catalog/configuration plan before
	// serving clients reconnecting to a reused port.
	DeferredStart bool
	// CatalogRemote is opt-in for custom providers; native OpenAI only accepts
	// catalog URLs on allowed ChatGPT hosts. Local JSON remains the default.
	CatalogMode CatalogMode
	// Independent selects a custom provider with its own loopback credential,
	// rather than sharing OpenAI's provider/account state in the desktop app.
	Independent bool
	Desktop     bool
	// ListenAddress preserves the desktop endpoint across daemon replacement.
	// It may only name an explicit IPv4 loopback port; empty chooses a free one.
	ListenAddress string
	// Native upstream overrides are for local integration fixtures. Commands
	// never populate them from client requests or environment variables.
	OpenAIURL  string
	ChatGPTURL string
}

type Route struct {
	Provider        string
	Alias           string
	Model           string
	NativeResponses bool
	MaxOutputTokens int
}

type Gateway struct {
	runtime          *oauthproxy.Runtime
	listener         net.Listener
	server           *http.Server
	client           *http.Client
	token            string
	routes           map[string]Route
	models           []Model
	modelSnapshot    atomic.Pointer[catalogSnapshot]
	catalogMu        sync.Mutex
	options          Options
	history          responseHistory
	migrations       responseHistory
	migrationNotices responseHistory
	// Only compaction of a catalogued previous native model may use the
	// independent provider's selected default. Normal unknown routes still fail.
	legacyCompaction map[string]Route
	closeOnce        sync.Once
	startOnce        sync.Once
	generation       *runtimeGeneration
	nativeCatalog    Catalog
}

func randomID(prefix string) string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// New initially borrows the runtime. OwnRuntime/ReplaceProviders transfer
// lifecycle cleanup to request-leased generations while the listener stays put.
func New(ctx context.Context, runtime *oauthproxy.Runtime, configured provider.Provider, options Options) (*Gateway, error) {
	if _, err := ParseCatalogMode(string(options.CatalogMode)); err != nil {
		return nil, err
	}
	options.CatalogMode = options.CatalogMode.Effective()
	if options.CatalogMode == CatalogRemote && !options.Independent {
		return nil, fmt.Errorf("remote CCL catalogs require independent mode; shared mode uses local JSON")
	}
	if err := provider.ValidateRuntime(configured); err != nil {
		return nil, err
	}
	address := options.ListenAddress
	if address == "" {
		address = "127.0.0.1:0"
	} else {
		host, port, err := net.SplitHostPort(address)
		n, portErr := strconv.Atoi(port)
		if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("Codex listener requires an explicit 127.0.0.1 port")
		}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // A local credential must never go through a shell proxy.
	g := &Gateway{runtime: runtime, listener: listener, options: options, token: randomID("ccl_"),
		routes: make(map[string]Route), client: &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := g.buildModels(runtime, configured); err != nil {
		_ = listener.Close()
		return nil, err
	}
	g.server = &http.Server{Handler: http.HandlerFunc(g.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	g.modelSnapshot.Store(g.currentLocked())
	if !options.DeferredStart {
		g.Start()
	}
	return g, nil
}

func (g *Gateway) Start() {
	g.startOnce.Do(func() { go func() { _ = g.server.Serve(g.listener) }() })
}

// buildModels only prepares an unpublished route/catalog pair.
func (g *Gateway) buildModels(runtime *oauthproxy.Runtime, configured provider.Provider) error {
	var err error
	p := configured
	p.Type = provider.EffectiveType(p)
	oauth := strings.ToLower(strings.TrimSpace(p.OAuthProvider))
	mixed := oauth == "" && len(p.ModelProtocols) > 0
	native := oauth == "gpt" || oauth == "codex" || oauth == "grok" || oauth == "xai" ||
		oauth == "" && !mixed && provider.IsOpenAIResponsesType(p.Type)
	details, names := runtime.ModelDetails(), runtime.ModelDisplayNames()
	models := runtime.Models()
	if !native {
		details, err = withModelsDevReasoning(models, details, names, p)
		if err != nil {
			return err
		}
	}
	details = withGPTReasoningDefaults(models, details)
	for _, model := range models {
		alias := "ccl/" + url.PathEscape(p.Name) + "/" + model
		info, ok := details[model]
		if !ok {
			info = details[strings.ToLower(model)]
		}
		modelProtocol := p.ModelProtocols[strings.ToLower(model)]
		if modelProtocol == "" {
			modelProtocol = p.ModelProtocols[strings.TrimSuffix(strings.ToLower(model), "[1m]")]
		}
		modelNative := native || mixed && provider.IsOpenAIResponsesType(modelProtocol)
		g.routes[alias] = Route{Provider: p.Name, Alias: alias, Model: model, NativeResponses: modelNative, MaxOutputTokens: info.MaxOutputTokens}
		entry := newModel(alias, model, details, names)
		if modelNative {
			entry["supports_search_tool"] = false
		}
		applyProviderContext(entry, model, p)
		g.models = append(g.models, entry)
	}
	if p.OAuthProvider == oauthproxy.ProviderWorkBuddy || p.OAuthProvider == oauthproxy.ProviderWorkBuddyCN {
		g.models = uniqueWorkBuddyModels(g.models, g.routes)
	}
	disambiguateModelNames(g.models, g.routes)
	if len(g.models) == 0 {
		return fmt.Errorf("provider %q has no models for Codex", p.Name)
	}
	return nil
}

func (g *Gateway) BaseURL() string { return "http://" + g.listener.Addr().String() + "/v1" }
func (g *Gateway) Token() string   { return g.token }
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		_ = g.server.Close()
		// Close may run before Serve registers its listener with the server.
		// Release it synchronously so the replacement can reuse this port.
		_ = g.listener.Close()
		g.client.CloseIdleConnections()
		g.catalogMu.Lock()
		gen := g.currentLocked().generation
		closeRuntime := retireGeneration(gen)
		g.catalogMu.Unlock()
		if closeRuntime != nil {
			closeRuntime()
		}
	})
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || r.Host != g.listener.Addr().String() || r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "permission_error", "loopback client required", "")
		return
	}
	if !g.options.Desktop && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid local CCL key", "")
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error", "CCL uses HTTP Responses; retry over HTTP", "")
		return
	}
	snapshot, release := g.acquireSnapshot()
	defer release()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if r.Method == http.MethodGet && (path == "/models" || path == "/codex/models") {
		g.serveCatalog(w, r, snapshot)
		return
	}
	if r.Method != http.MethodPost || (path != "/responses" && path != "/responses/compact") {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unsupported Codex endpoint", "")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	var request responseRequest
	if err := json.Unmarshal(body, &request); err != nil || request.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "a Responses model is required", "")
		return
	}
	route, ok := snapshot.routes[request.Model]
	if !ok && g.options.Independent {
		items, _ := inputItems(request.Input)
		if isClientCompaction(r, request, items, path) {
			if replacement, allowed := snapshot.legacyCompaction[request.Model]; allowed {
				route, ok = replacement, true
				request.Model = route.Alias
				request.Reasoning = nil
				for _, model := range snapshot.models {
					if model["slug"] == route.Alias {
						if effort, _ := model["default_reasoning_level"].(string); effort != "" {
							request.Reasoning = map[string]any{"effort": effort}
						}
						break
					}
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(body, &fields)
				fields["model"], _ = json.Marshal(request.Model)
				if request.Reasoning == nil {
					delete(fields, "reasoning")
				} else {
					fields["reasoning"], _ = json.Marshal(request.Reasoning)
				}
				body, _ = json.Marshal(fields)
			}
		}
	}
	if !ok {
		if !g.options.Desktop || strings.HasPrefix(strings.ToLower(request.Model), "ccl/") {
			name := request.Model
			if len(name) > 128 {
				name = name[:128] + "..."
			}
			writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("model is not in the CCL routing catalog: %q; select a model from the active CCL provider or switch Codex back to the chat's provider", name), "model_not_found")
			return
		}
		g.serveNative(w, r, nativeHistory(body), path)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), runtimeRequestKey{}, snapshot.runtimes[route.Alias]))
	if route.NativeResponses {
		g.forwardResponses(w, r, nativeHistory(body), route, path)
		return
	}
	g.serveBridge(w, r, body, request, route, path == "/responses/compact")
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var reader io.Reader = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "zstd":
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(maxBodyBytes*2))
		if err != nil {
			return nil, fmt.Errorf("invalid zstd request")
		}
		defer decoder.Close()
		reader = decoder
	default:
		return nil, fmt.Errorf("unsupported request content encoding")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return nil, fmt.Errorf("Responses request exceeds limit or cannot be decoded")
	}
	return body, nil
}

func (g *Gateway) forwardResponses(w http.ResponseWriter, r *http.Request, body []byte, route Route, path string) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	fields["model"], _ = json.Marshal(route.Model)
	body, _ = json.Marshal(fields)
	response, err := g.callRuntime(r, body, path)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "CCL provider connection failed", "")
		return
	}
	defer response.Body.Close()
	relayResponse(w, response)
}

func (g *Gateway) callRuntime(r *http.Request, body []byte, path string) (*http.Response, error) {
	runtime := g.requestRuntime(r)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, runtime.Endpoint()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream, application/json")
	for _, key := range []string{"Session_id", "X-Codex-Turn-Metadata", "X-Codex-Session-Id"} {
		if value := r.Header.Get(key); value != "" {
			request.Header.Set(key, value)
		}
	}
	return g.client.Do(request)
}

func (g *Gateway) serveNative(w http.ResponseWriter, r *http.Request, body []byte, path string) {
	auth := r.Header.Get("Authorization")
	if auth == "" || auth == "Bearer "+g.token {
		writeError(w, http.StatusUnauthorized, "authentication_error", "native OpenAI models require Codex's own login", "")
		return
	}
	target := g.options.OpenAIURL
	if target == "" {
		target = "https://api.openai.com/v1"
	}
	if r.Header.Get("Chatgpt-Account-Id") != "" {
		target = g.options.ChatGPTURL
		if target == "" {
			target = "https://chatgpt.com/backend-api/codex"
		}
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		writeError(w, http.StatusBadGateway, "api_error", "invalid native upstream", "")
		return
	}
	proxy := &httputil.ReverseProxy{FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.URL.Path = strings.TrimRight(u.Path, "/") + path
		pr.Out.Host = ""
		pr.Out.Body = io.NopCloser(bytes.NewReader(body))
		pr.Out.ContentLength = int64(len(body))
		pr.Out.Header.Del("Content-Encoding")
		pr.Out.Header.Del("Content-Length")
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeError(w, http.StatusBadGateway, "api_error", "native OpenAI connection failed", "")
	}}
	proxy.ServeHTTP(w, r)
}

func relayResponse(w http.ResponseWriter, response *http.Response) {
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := response.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flushErr := http.NewResponseController(w).Flush(); flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, kind, message, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message, "code": code, "param": nil}})
}

func historyOwner(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:])
}
