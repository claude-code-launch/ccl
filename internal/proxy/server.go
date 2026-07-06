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

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type Server struct {
	addr            string
	provider        provider.Provider
	logger          *slog.Logger
	httpServer      *http.Server
	ln              net.Listener
	wg              sync.WaitGroup
	availableModels string
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

func (s *Server) AvailableModels() string {
	return s.availableModels
}

func (s *Server) fetchAvailableModels() {
	availModels, err := protocol.GetOpenAIModels(s.provider.Endpoint, s.provider.APIKey)
	if err != nil {
		s.logger.Error("Dynamically discovered available gateway models err", "error", err)
		return
	}
	s.availableModels = availModels

	s.logger.Info("Dynamically discovered available gateway models", "count", len(s.availableModels), "models", s.availableModels)
}

func (s *Server) modelPool() []string {
	models := s.provider.Model
	if models == "" {
		models = s.availableModels
	}
	return modelrouting.SplitCSV(models)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	s.logger.Debug("Received Anthropic Messages request")

	var antReq protocol.AnthropicRequest

	if err := json.NewDecoder(r.Body).Decode(&antReq); err != nil {
		s.logger.Error("Failed to parse request body as AnthropicRequest", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Resolve incoming model alias back to real gateway model name
	modelPool := s.modelPool()
	realModel := protocol.FromGatewayModelAlias(antReq.Model, modelPool)
	// If it matches pool models as configured, or if FromGatewayModelAlias resolved it, use that
	mappedModel := protocol.MapModel(realModel, s.provider.Model, modelPool)

	s.logger.Info("Mapped requested model to target gateway model", "requested", antReq.Model, "resolved_alias", realModel, "mapped", mappedModel)

	openAIResponses := provider.IsOpenAIResponsesType(s.provider.Type)
	upstreamBody, endpoint, err := s.convertAnthropicRequestForUpstream(&antReq, mappedModel, openAIResponses)
	if err != nil {
		s.logger.Error("Failed to translate Anthropic request", "error", err)
		http.Error(w, "Internal Translation Error", http.StatusInternalServerError)
		return
	}
	s.logger.Info("Mapped requested model to target gateway model", "requested", antReq.Model, "mapped", mappedModel)

	// Forward Request to Target Endpoint (OpenAI-compatible)
	client := &http.Client{}
	oaBody, err := json.Marshal(upstreamBody)
	if err != nil {
		s.logger.Error("Failed to encode upstream request", "error", err)
		http.Error(w, "Internal Encoding Error", http.StatusInternalServerError)
		return
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
		if openAIResponses {
			s.handleResponsesStreaming(w, resp.Body)
		} else {
			s.handleStreaming(w, resp.Body)
		}
		return
	}

	// Non-streaming response handling
	if openAIResponses {
		s.handleResponsesUnary(w, resp.Body)
	} else {
		s.handleUnary(w, resp.Body)
	}
}

func (s *Server) convertAnthropicRequestForUpstream(antReq *protocol.AnthropicRequest, mappedModel string, responses bool) (any, string, error) {
	if responses {
		req, err := protocol.ConvertRequestToResponses(antReq)
		if err != nil {
			return nil, "", err
		}
		req.Model = mappedModel
		return req, protocol.NormalizeOpenAIResponsesURL(s.provider.Endpoint), nil
	}

	req, err := protocol.ConvertRequest(antReq)
	if err != nil {
		return nil, "", err
	}
	req.Model = mappedModel
	return req, protocol.NormalizeOpenAIChatCompletionsURL(s.provider.Endpoint), nil
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

func (s *Server) handleResponsesUnary(w http.ResponseWriter, body io.Reader) {
	respBytes, err := io.ReadAll(body)
	if err != nil {
		s.logger.Error("Failed to read upstream Responses response", "error", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	var oaResp protocol.OpenAIResponsesResponse
	if err := json.Unmarshal(respBytes, &oaResp); err != nil {
		s.logger.Error("Failed to parse OpenAI Responses response", "error", err)
		http.Error(w, "Internal Mapping Error", http.StatusInternalServerError)
		return
	}

	antResp, err := protocol.ConvertResponsesResponse(&oaResp)
	if err != nil {
		s.logger.Error("Failed to convert OpenAI Responses response to Anthropic style", "error", err)
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
	if formatted := FormatEvents(st.Finish()); formatted != "" {
		if _, err := fmt.Fprint(w, formatted); err != nil {
			s.logger.Error("Failed to write final SSE event to client", "error", err)
			return
		}
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("Error scanning streaming input", "error", err)
	}
}

func (s *Server) handleResponsesStreaming(w http.ResponseWriter, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Error("ResponseWriter does not support Flushing")
		http.Error(w, "Streaming Unsupported", http.StatusInternalServerError)
		return
	}

	st := &ResponsesStreamTransformer{}
	scanner := bufio.NewScanner(body)
	var block strings.Builder

	flushBlock := func() bool {
		raw := strings.TrimSpace(block.String())
		block.Reset()
		if raw == "" {
			return true
		}
		events, err := st.TranslateBlock(raw)
		if err != nil {
			s.logger.Error("Error parsing Responses stream block", "error", err)
			return true
		}
		formatted := FormatEvents(events)
		if formatted == "" {
			return true
		}
		if _, err := fmt.Fprint(w, formatted); err != nil {
			s.logger.Error("Failed to write SSE event to client", "error", err)
			return false
		}
		flusher.Flush()
		return true
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if !flushBlock() {
				return
			}
			continue
		}
		block.WriteString(line)
		block.WriteByte('\n')
	}
	if block.Len() > 0 {
		flushBlock()
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("Error scanning Responses streaming input", "error", err)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {

	s.logger.Debug("Received models check request")
	w.Header().Set("Content-Type", "application/json")

	models := protocol.BatchToGatewayModelAlias(s.modelPool())

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
