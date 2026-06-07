package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/haiboyuwen/claude-code-launch/internal/protocol"
	"github.com/haiboyuwen/claude-code-launch/internal/provider"
)

type Server struct {
	addr            string
	provider        provider.Provider
	logger          *slog.Logger
	httpServer      *http.Server
	ln              net.Listener
	wg              sync.WaitGroup
	availableModels []string
	modelsMutex     sync.RWMutex
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func NewServer(addr string, p provider.Provider, logger *slog.Logger) *Server {
	return &Server{
		addr:     addr,
		provider: p,
		logger:   logger,
	}
}

// Start runs the local proxy server on the configured address.
// It is non-blocking and starts the server in a background goroutine.
func (s *Server) Start() error {
	var err error
	s.ln, err = net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to bind proxy to %s: %w", s.addr, err)
	}

	// Fetch models from the gateway synchronously if no model configured
	if s.provider.Model == "" {
		s.fetchAvailableModels()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/models", s.handleModels)
	// Fallback/catch-all
	mux.HandleFunc("/", s.handleFallback)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("Proxy server listening", "addr", s.ln.Addr().String())
		if err := s.httpServer.Serve(s.ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP proxy server error", "error", err)
		}
	}()

	return nil
}

// Stop shuts down the proxy server cleanly.
func (s *Server) Stop() {
	if s.httpServer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.Error("Error during proxy server shutdown", "error", err)
	}
	s.wg.Wait()
	s.logger.Info("Proxy server stopped")
}

// Addr returns the bound listener address (useful if port is randomly allocated).
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.addr
}

func (s *Server) AvailableModels() []string {
	s.modelsMutex.RLock()
	defer s.modelsMutex.RUnlock()
	if len(s.availableModels) == 0 {
		return nil
	}
	res := make([]string, len(s.availableModels))
	copy(res, s.availableModels)
	return res
}

func (s *Server) fetchAvailableModels() {
	endpoint := strings.TrimSuffix(s.provider.Endpoint, "/")
	if endpoint == "" {
		if s.provider.Type == "openai" {
			endpoint = "https://api.openai.com/v1"
		} else {
			endpoint = "https://api.anthropic.com"
		}
	}
	modelsURL := endpoint + "/models"
	if !strings.HasSuffix(endpoint, "/v1") {
		modelsURL = endpoint + "/v1/models"
		modelsURL = strings.Replace(modelsURL, "/v1/v1", "/v1", 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err != nil {
		s.logger.Warn("Failed to create models discovery request", "error", err)
		return
	}

	if s.provider.Type == "openai" {
		req.Header.Set("Authorization", "Bearer "+s.provider.APIKey)
	} else {
		req.Header.Set("x-api-key", s.provider.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("Models discovery request failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("Models discovery returned non-200 status", "status", resp.StatusCode)
		return
	}

	var mResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		s.logger.Warn("Failed to decode models response", "error", err)
		return
	}

	s.modelsMutex.Lock()
	s.availableModels = make([]string, 0, len(mResp.Data))
	for _, m := range mResp.Data {
		s.availableModels = append(s.availableModels, m.ID)
	}
	s.modelsMutex.Unlock()

	s.logger.Info("Dynamically discovered available gateway models", "count", len(s.availableModels), "models", s.availableModels)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	s.logger.Debug("Received Anthropic Messages request")

	// Read and decode the incoming Anthropic Request
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("Failed to read request body", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var antReq protocol.AnthropicRequest
	if err := json.Unmarshal(bodyBytes, &antReq); err != nil {
		s.logger.Error("Failed to parse request body as AnthropicRequest", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Resolve incoming model alias back to real gateway model name
	s.modelsMutex.RLock()
	realModel := protocol.FromGatewayModelAlias(antReq.Model, s.availableModels)
	// If it matches pool models as configured, or if FromGatewayModelAlias resolved it, use that
	mappedModel := protocol.MapModel(realModel, s.provider.Model, s.availableModels)
	s.modelsMutex.RUnlock()

	s.logger.Info("Mapped requested model to target gateway model", "requested", antReq.Model, "resolved_alias", realModel, "mapped", mappedModel)

	// -------------------------------------------------------------
	// Handle "anthropic" type provider: native passthrough
	// -------------------------------------------------------------
	if s.provider.Type == "anthropic" {
		antReq.Model = mappedModel
		antBody, err := json.Marshal(antReq)
		if err != nil {
			s.logger.Error("Failed to encode Anthropic request", "error", err)
			http.Error(w, "Internal Encoding Error", http.StatusInternalServerError)
			return
		}

		endpoint := strings.TrimSuffix(s.provider.Endpoint, "/")
		if endpoint == "" {
			endpoint = "https://api.anthropic.com"
		}
		if !strings.HasSuffix(endpoint, "/messages") && !strings.HasSuffix(endpoint, "/v1/messages") {
			endpoint = endpoint + "/v1/messages"
			endpoint = strings.Replace(endpoint, "/v1/v1/", "/v1/", 1)
		}

		s.logger.Debug("Forwarding native Anthropic request", "url", endpoint)

		reqCtx := r.Context()
		forwardReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewBuffer(antBody))
		if err != nil {
			s.logger.Error("Failed to create outgoing request", "error", err)
			http.Error(w, "Internal Routing Error", http.StatusInternalServerError)
			return
		}

		// Copy or set Anthropic specific headers
		forwardReq.Header.Set("Content-Type", "application/json")
		forwardReq.Header.Set("x-api-key", s.provider.APIKey)

		// Propagate Anthropic Version
		antVersion := r.Header.Get("anthropic-version")
		if antVersion == "" {
			antVersion = "2023-06-01"
		}
		forwardReq.Header.Set("anthropic-version", antVersion)

		// Propagate any client IP headers or other metadata if present
		if clientIP := r.Header.Get("x-forwarded-for"); clientIP != "" {
			forwardReq.Header.Set("x-forwarded-for", clientIP)
		}

		client := &http.Client{}
		resp, err := client.Do(forwardReq)
		if err != nil {
			s.logger.Error("Outgoing Anthropic request failed", "error", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Propagate upstream response headers back to client
		for k, v := range resp.Header {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}

		w.WriteHeader(resp.StatusCode)

		// Stream or write the native Anthropic response directly back
		// Since both endpoints speak the same protocol, io.Copy handles both unary and streaming SSE flawlessly.
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			s.logger.Error("Failed to stream response back to client", "error", err)
		}
		return
	}

	// -------------------------------------------------------------
	// Handle "openai" type provider: translation proxy
	// -------------------------------------------------------------

	// Transform Anthropic Request into OpenAI Request
	oaReq, err := protocol.ConvertRequest(&antReq)
	if err != nil {
		s.logger.Error("Failed to translate Anthropic request to OpenAI format", "error", err)
		http.Error(w, "Internal Translation Error", http.StatusInternalServerError)
		return
	}

	oaReq.Model = mappedModel
	s.logger.Info("Mapped requested model to target gateway model", "requested", antReq.Model, "mapped", mappedModel)

	// Forward Request to Target Endpoint (OpenAI-compatible)
	client := &http.Client{}
	oaBody, err := json.Marshal(oaReq)
	if err != nil {
		s.logger.Error("Failed to encode OpenAI request", "error", err)
		http.Error(w, "Internal Encoding Error", http.StatusInternalServerError)
		return
	}

	// The provider's Endpoint might not end with "/v1/chat/completions" — let's normalize.
	endpoint := strings.TrimSuffix(s.provider.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		// Append appropriate suffix for OpenAI
		endpoint = endpoint + "/v1/chat/completions"
		// Make sure we don't end up with /v1/v1/chat/completions
		endpoint = strings.Replace(endpoint, "/v1/v1/", "/v1/", 1)
	}

	s.logger.Debug("Forwarding converted request to OpenAI endpoint", "url", endpoint)

	reqCtx := r.Context()
	forwardReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewBuffer(oaBody))
	if err != nil {
		s.logger.Error("Failed to create outgoing request", "error", err)
		http.Error(w, "Internal Routing Error", http.StatusInternalServerError)
		return
	}

	// Set headers
	forwardReq.Header.Set("Content-Type", "application/json")
	forwardReq.Header.Set("Authorization", "Bearer "+s.provider.APIKey)

	// Perform the HTTP Request
	resp, err := client.Do(forwardReq)
	if err != nil {
		s.logger.Error("Outgoing request failed", "error", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		s.logger.Error("Upstream returned non-200 status", "status", resp.StatusCode, "body", string(respBody))
		// Pipe the exact error details back
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// Check if streaming is requested
	if antReq.Stream {
		s.handleStreaming(w, resp.Body)
		return
	}

	// Non-streaming response handling
	s.handleUnary(w, resp.Body)
}

func (s *Server) handleUnary(w http.ResponseWriter, body io.Reader) {
	respBytes, err := io.ReadAll(body)
	if err != nil {
		s.logger.Error("Failed to read upstream response", "error", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	var oaResp protocol.OpenAIResponse
	if err := json.Unmarshal(respBytes, &oaResp); err != nil {
		s.logger.Error("Failed to parse OpenAI response", "error", err)
		http.Error(w, "Internal Mapping Error", http.StatusInternalServerError)
		return
	}

	antResp, err := protocol.ConvertResponse(&oaResp)
	if err != nil {
		s.logger.Error("Failed to convert OpenAI response to Anthropic style", "error", err)
		http.Error(w, "Internal Mapping Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(antResp)
}

func (s *Server) handleStreaming(w http.ResponseWriter, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Error("ResponseWriter does not support Flushing")
		http.Error(w, "Streaming Unsupported", http.StatusInternalServerError)
		return
	}

	st := &StreamTransformer{}
	scanner := bufio.NewScanner(body)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		events, err := st.TranslateChunk(line)
		if err != nil {
			s.logger.Error("Error parsing stream line", "error", err)
			continue
		}

		if len(events) > 0 {
			formatted := FormatEvents(events)
			if formatted != "" {
				_, err := fmt.Fprint(w, formatted)
				if err != nil {
					s.logger.Error("Failed to write SSE event to client", "error", err)
					return
				}
				flusher.Flush()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("Error scanning streaming input", "error", err)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("Received models check request")
	w.Header().Set("Content-Type", "application/json")

	standardModels := []string{
		"claude-sonnet-4-6",
		"claude-sonnet-4-6-20250514",
		"claude-3-5-sonnet",
		"claude-3-5-sonnet-20241022",
		"claude-opus-4-8",
		"claude-opus-4-8-20250514",
		"claude-opus-4-7",
		"claude-opus-4-7-20250514",
		"claude-3-opus",
		"claude-3-opus-20240229",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
		"claude-3-5-haiku",
		"claude-3-5-haiku-20241022",
	}

	poolModels := []string{}
	if s.provider.Model != "" && strings.Contains(s.provider.Model, ",") {
		for _, m := range strings.Split(s.provider.Model, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				poolModels = append(poolModels, m)
			}
		}
	}

	s.modelsMutex.RLock()
	availModels := s.availableModels
	s.modelsMutex.RUnlock()

	// Collect models from all sources and deduplicate them, mapping non-compliant ones to aliases.
	var models []string
	seen := make(map[string]bool)

	addModel := func(m string) {
		if m != "" {
			alias := protocol.ToGatewayModelAlias(m)
			if !seen[alias] {
				seen[alias] = true
				models = append(models, alias)
			}
		}
	}

	// 1. First add pool models if configured
	for _, m := range poolModels {
		addModel(m)
	}

	// 2. Next add dynamically discovered models from the gateway
	for _, m := range availModels {
		addModel(m)
	}

	// 3. If still empty, fall back to standard models
	if len(models) == 0 {
		for _, m := range standardModels {
			addModel(m)
		}
	}

	var buf bytes.Buffer
	buf.WriteString(`{"data":[`)
	first := true
	writeModel := func(id string) {
		if !first {
			buf.WriteString(",")
		}
		first = false
		buf.WriteString(fmt.Sprintf(`{"id":"%s","type":"model"}`, id))
	}
	for _, id := range models {
		writeModel(id)
	}
	buf.WriteString(`]}`)

	w.Write(buf.Bytes())
}

func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("Fallback catch-all route triggered", "path", r.URL.Path, "method", r.Method)
	http.Error(w, "Endpoint Not Supported by Local Proxy", http.StatusNotFound)
}
