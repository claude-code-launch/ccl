package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/haiboyuwen/cc/internal/protocol"
	"github.com/haiboyuwen/cc/internal/provider"
	"go.uber.org/zap"
)

type Server struct {
	addr            string
	provider        provider.Provider
	logger          *zap.Logger
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

func NewServer(addr string, p provider.Provider, logger *zap.Logger) *Server {
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

	// Fetch models from the gateway in a non-blocking background task (only if no model configured)
	if s.provider.Model == "" {
		go s.fetchAvailableModels()
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
		s.logger.Info("Proxy server listening", zap.String("addr", s.ln.Addr().String()))
		if err := s.httpServer.Serve(s.ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP proxy server error", zap.Error(err))
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
		s.logger.Error("Error during proxy server shutdown", zap.Error(err))
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

func (s *Server) fetchAvailableModels() {
	endpoint := strings.TrimSuffix(s.provider.Endpoint, "/")
	modelsURL := endpoint + "/models"
	if !strings.HasSuffix(endpoint, "/v1") {
		modelsURL = endpoint + "/v1/models"
		modelsURL = strings.Replace(modelsURL, "/v1/v1", "/v1", 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err != nil {
		s.logger.Warn("Failed to create models discovery request", zap.Error(err))
		return
	}

	req.Header.Set("Authorization", "Bearer "+s.provider.APIKey)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("Models discovery request failed", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("Models discovery returned non-200 status", zap.Int("status", resp.StatusCode))
		return
	}

	var mResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		s.logger.Warn("Failed to decode models response", zap.Error(err))
		return
	}

	s.modelsMutex.Lock()
	s.availableModels = make([]string, 0, len(mResp.Data))
	for _, m := range mResp.Data {
		s.availableModels = append(s.availableModels, m.ID)
	}
	s.modelsMutex.Unlock()

	s.logger.Info("Dynamically discovered available gateway models", zap.Int("count", len(s.availableModels)), zap.Strings("models", s.availableModels))
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
		s.logger.Error("Failed to read request body", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var antReq protocol.AnthropicRequest
	if err := json.Unmarshal(bodyBytes, &antReq); err != nil {
		s.logger.Error("Failed to parse request body as AnthropicRequest", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Transform Anthropic Request into OpenAI Request
	oaReq, err := protocol.ConvertRequest(&antReq)
	if err != nil {
		s.logger.Error("Failed to translate Anthropic request to OpenAI format", zap.Error(err))
		http.Error(w, "Internal Translation Error", http.StatusInternalServerError)
		return
	}

	// Dynamic Model Mapping: intelligent mapping of the requested Anthropic model (e.g., opus, sonnet, haiku)
	s.modelsMutex.RLock()
	mappedModel := protocol.MapModel(antReq.Model, s.provider.Model, s.availableModels)
	s.modelsMutex.RUnlock()

	oaReq.Model = mappedModel
	s.logger.Info("Mapped requested model to target gateway model", zap.String("requested", antReq.Model), zap.String("mapped", mappedModel))

	// Forward Request to Target Endpoint (OpenAI-compatible)
	client := &http.Client{}
	oaBody, err := json.Marshal(oaReq)
	if err != nil {
		s.logger.Error("Failed to encode OpenAI request", zap.Error(err))
		http.Error(w, "Internal Encoding Error", http.StatusInternalServerError)
		return
	}

	// The provider's Endpoint might not end with "/v1/chat/completions" — let's normalize.
	endpoint := strings.TrimSuffix(s.provider.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		// Append appropriate suffix for OpenAI
		endpoint = endpoint + "/v1/chat/completions"
		// Make sure we don't end up with /v1/v1/chat/completions
		endpoint = strings.Replace(endpoint, "/v1/v1/", "/v1/", 1)
	}

	s.logger.Debug("Forwarding converted request to OpenAI endpoint", zap.String("url", endpoint))

	reqCtx := r.Context()
	forwardReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewBuffer(oaBody))
	if err != nil {
		s.logger.Error("Failed to create outgoing request", zap.Error(err))
		http.Error(w, "Internal Routing Error", http.StatusInternalServerError)
		return
	}

	// Set headers
	forwardReq.Header.Set("Content-Type", "application/json")
	forwardReq.Header.Set("Authorization", "Bearer "+s.provider.APIKey)

	// Perform the HTTP Request
	resp, err := client.Do(forwardReq)
	if err != nil {
		s.logger.Error("Outgoing request failed", zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		s.logger.Error("Upstream returned non-200 status", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
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
		s.logger.Error("Failed to read upstream response", zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	var oaResp protocol.OpenAIResponse
	if err := json.Unmarshal(respBytes, &oaResp); err != nil {
		s.logger.Error("Failed to parse OpenAI response", zap.Error(err))
		http.Error(w, "Internal Mapping Error", http.StatusInternalServerError)
		return
	}

	antResp, err := protocol.ConvertResponse(&oaResp)
	if err != nil {
		s.logger.Error("Failed to convert OpenAI response to Anthropic style", zap.Error(err))
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
			s.logger.Error("Error parsing stream line", zap.Error(err))
			continue
		}

		if len(events) > 0 {
			formatted := FormatEvents(events)
			if formatted != "" {
				_, err := fmt.Fprint(w, formatted)
				if err != nil {
					s.logger.Error("Failed to write SSE event to client", zap.Error(err))
					return
				}
				flusher.Flush()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("Error scanning streaming input", zap.Error(err))
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("Received models check request")
	// Return a stub Anthropic compatible response for model checks.
	w.Header().Set("Content-Type", "application/json")
	modelsJSON := `{
		"data": [
			{
				"id": "claude-3-5-sonnet",
				"type": "model"
			}
		]
	}`
	w.Write([]byte(modelsJSON))
}

func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("Fallback catch-all route triggered", zap.String("path", r.URL.Path), zap.String("method", r.Method))
	http.Error(w, "Endpoint Not Supported by Local Proxy", http.StatusNotFound)
}
