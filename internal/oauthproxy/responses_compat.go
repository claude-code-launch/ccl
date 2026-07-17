package oauthproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const plainResponsesUserAgent = "ccl-openai-responses/1.0"

// responsesCompatibilityProxy is a loopback reverse proxy placed in front of
// the real Responses/Codex upstream (see package doc, item 1).
//
// It leaves Responses traffic mostly untouched except for CLIProxyAPI edge cases:
// some upstreams emit the full answer only in the final response.completed
// event, and some omit or delay response.created. Drop this once the SDK
// handles both cases natively.
type responsesCompatibilityProxy struct {
	endpoint string
	server   *http.Server
	done     chan struct{}
}

type responsesEvent struct {
	Type     string              `json:"type"`
	Response responsesEventBody  `json:"response"`
	Item     responsesOutputItem `json:"item"`
	Delta    string              `json:"delta"`
}

type responsesEventBody struct {
	ID     string                `json:"id"`
	Model  string                `json:"model"`
	Output []responsesOutputItem `json:"output"`
}

type responsesOutputItem struct {
	Type    string                 `json:"type"`
	Content []responsesContentPart `json:"content"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// startResponsesCompatibilityProxy fronts a Responses upstream.
// When identity is non-nil, Codex client identity is injected into each request.
// When identity is nil, residual Codex headers/body fields that CLIProxyAPI's
// codex-api-key executor always injects are stripped so plain OpenAI Responses
// gateways are not forced into Codex-only parameters.
func startResponsesCompatibilityProxy(targetEndpoint string, identity *codexRequestIdentity) (*responsesCompatibilityProxy, error) {
	target, err := url.Parse(strings.TrimRight(strings.TrimSpace(targetEndpoint), "/"))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid Responses endpoint %q", targetEndpoint)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start Responses compatibility listener: %w", err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			if identity != nil {
				_ = normalizeCodexRequestIdentity(request.Out, *identity)
			} else {
				_ = sanitizePlainResponsesRequest(request.Out)
			}
			request.SetURL(target)
			request.Out.Host = target.Host
		},
		ModifyResponse: normalizeCompletedOnlyResponses,
		ErrorLog:       log.New(io.Discard, "", 0),
	}
	server := &http.Server{Handler: proxy}
	compat := &responsesCompatibilityProxy{
		endpoint: "http://" + listener.Addr().String(),
		server:   server,
		done:     make(chan struct{}),
	}
	go func() {
		_ = server.Serve(listener)
		close(compat.done)
	}()
	return compat, nil
}

func normalizeCodexRequestIdentity(request *http.Request, identity codexRequestIdentity) error {
	if request == nil || request.Body == nil || request.Method != http.MethodPost ||
		!strings.Contains(strings.ToLower(request.URL.Path), "responses") {
		return nil
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		request.Body = io.NopCloser(bytes.NewReader(body))
		return err
	}

	sessionID, _ := payload["prompt_cache_key"].(string)
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = codexSessionHeader(request.Header)
	}
	if sessionID == "" {
		sessionID = identity.sessionID
	}
	windowID := sessionID + ":0"
	turnMetadata, err := json.Marshal(map[string]any{
		"installation_id":         identity.installationID,
		"session_id":              sessionID,
		"thread_id":               sessionID,
		"turn_id":                 identity.turnID,
		"window_id":               windowID,
		"request_kind":            "turn",
		"thread_source":           "user",
		"sandbox":                 "seatbelt",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}

	clientMetadata, _ := payload["client_metadata"].(map[string]any)
	if clientMetadata == nil {
		clientMetadata = make(map[string]any)
	}
	clientMetadata["x-codex-installation-id"] = identity.installationID
	clientMetadata["session_id"] = sessionID
	clientMetadata["thread_id"] = sessionID
	clientMetadata["turn_id"] = identity.turnID
	clientMetadata["x-codex-window-id"] = windowID
	clientMetadata["x-codex-turn-metadata"] = string(turnMetadata)
	payload["client_metadata"] = clientMetadata
	payload["prompt_cache_key"] = sessionID

	body, err = json.Marshal(payload)
	if err != nil {
		return err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Del("Content-Length")

	deleteCodexIdentityHeaders(request.Header)
	request.Header.Set("Session-Id", sessionID)
	request.Header.Set("Thread-Id", sessionID)
	request.Header.Set("X-Client-Request-Id", sessionID)
	request.Header.Set("X-Codex-Window-Id", windowID)
	request.Header.Set("X-Codex-Turn-Metadata", string(turnMetadata))
	return nil
}

// sanitizePlainResponsesRequest removes Codex-only headers and body fields that
// CLIProxyAPI's codex-api-key executor injects even for API-key credentials.
// Plain OpenAI Responses gateways commonly reject those as unsupported.
func sanitizePlainResponsesRequest(request *http.Request) error {
	if request == nil {
		return nil
	}

	deleteCodexIdentityHeaders(request.Header)
	for key := range request.Header {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
		switch normalized {
		case "originator", "x-codex-beta-features", "chatgpt-account-id", "version":
			delete(request.Header, key)
		}
	}
	// CLIProxyAPI defaults User-Agent to codex-tui/... (Mac OS ...); that also
	// triggers Session_id injection. Always replace with a neutral UA.
	request.Header.Set("User-Agent", plainResponsesUserAgent)

	if request.Body == nil || request.Method != http.MethodPost ||
		!strings.Contains(strings.ToLower(request.URL.Path), "responses") {
		return nil
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		request.Body = io.NopCloser(bytes.NewReader(body))
		return err
	}
	delete(payload, "client_metadata")
	// prompt_cache_key is valid OpenAI Responses, but when it was only injected
	// as a Codex session key it is safer to leave user-supplied values alone.
	// Do not invent one for plain mode.

	body, err = json.Marshal(payload)
	if err != nil {
		return err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Del("Content-Length")
	return nil
}

func codexSessionHeader(headers http.Header) string {
	for key, values := range headers {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
		if normalized == "session-id" && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func deleteCodexIdentityHeaders(headers http.Header) {
	for key := range headers {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
		switch normalized {
		case "session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata", "conversation-id":
			delete(headers, key)
		}
	}
}

func normalizeCompletedOnlyResponses(response *http.Response) error {
	if response == nil || response.Body == nil ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil
	}

	original := response.Body
	reader, writer := io.Pipe()
	response.Body = reader
	response.ContentLength = -1
	response.Header.Del("Content-Length")
	go recoverCompletedOnlyText(original, writer)
	return nil
}

func recoverCompletedOnlyText(source io.ReadCloser, destination *io.PipeWriter) {
	defer source.Close()
	scanner := bufio.NewScanner(source)
	scanner.Buffer(nil, 52_428_800)
	seenCreated := false
	syntheticCreated := false
	seenText := false
	// responseID/model accumulate from any event that carries them so a
	// synthetic response.created can be emitted before the first content
	// event when upstreams skip the normal created frame.
	responseID := ""
	responseModel := ""
	var pendingEventLine []byte

	writeLine := func(line []byte) error {
		if _, err := destination.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return err
		}
		return nil
	}

	flushPendingEvent := func() error {
		if pendingEventLine == nil {
			return nil
		}
		if err := writeLine(pendingEventLine); err != nil {
			return err
		}
		pendingEventLine = nil
		return nil
	}

	dropPendingEvent := func() {
		pendingEventLine = nil
	}

	ensureCreated := func(id, model string) error {
		if seenCreated {
			return nil
		}
		if id == "" {
			id = responseID
		}
		if model == "" {
			model = responseModel
		}
		if id == "" {
			id = "resp_synthetic"
		}
		created, _ := json.Marshal(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":    id,
				"model": model,
			},
		})
		// Emit created before any held event: line for the content frame so
		// CLIProxyAPI sees message_start before content_block events.
		if _, err := fmt.Fprintf(destination, "data: %s\n\n", created); err != nil {
			return err
		}
		seenCreated = true
		syntheticCreated = true
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := strings.TrimSpace(string(line))

		// Hold event: lines until we know whether the following data: is dropped.
		if strings.HasPrefix(trimmed, "event:") {
			if err := flushPendingEvent(); err != nil {
				_ = destination.CloseWithError(err)
				return
			}
			pendingEventLine = append([]byte(nil), line...)
			continue
		}

		dropData := false
		if payload, ok := strings.CutPrefix(trimmed, "data:"); ok {
			payload = strings.TrimSpace(payload)
			var event responsesEvent
			if json.Unmarshal([]byte(payload), &event) == nil {
				if event.Response.ID != "" {
					responseID = event.Response.ID
				}
				if event.Response.Model != "" {
					responseModel = event.Response.Model
				}
				switch event.Type {
				case "response.created":
					if seenCreated {
						// Already emitted synthetic (or real) created — drop
						// a late real created so CLIProxyAPI does not emit a
						// second message_start.
						dropData = true
					} else {
						seenCreated = true
						syntheticCreated = false
					}
				case "response.output_text.delta":
					if err := ensureCreated(responseID, responseModel); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
					if event.Delta != "" {
						seenText = true
					}
				case "response.output_item.done", "response.output_item.added":
					if err := ensureCreated(responseID, responseModel); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
					if outputItemText(event.Item) != "" {
						seenText = true
					}
				case "response.completed", "response.incomplete":
					if err := ensureCreated(event.Response.ID, event.Response.Model); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
					if !seenText {
						text := outputItemsText(event.Response.Output)
						if text != "" {
							delta, _ := json.Marshal(map[string]any{
								"type":  "response.output_text.delta",
								"delta": text,
							})
							if _, err := fmt.Fprintf(destination, "data: %s\n\n", delta); err != nil {
								_ = destination.CloseWithError(err)
								return
							}
							seenText = true
						}
					}
				default:
					// Other events (e.g. function_call deltas) also need a
					// preceding created frame for CLIProxyAPI's translator.
					if !seenCreated && event.Type != "" {
						if err := ensureCreated(responseID, responseModel); err != nil {
							_ = destination.CloseWithError(err)
							return
						}
					}
				}
			}
		}

		if dropData {
			dropPendingEvent()
			_ = syntheticCreated // retained for tests / future diagnostics
			continue
		}
		if err := flushPendingEvent(); err != nil {
			_ = destination.CloseWithError(err)
			return
		}
		if err := writeLine(line); err != nil {
			_ = destination.CloseWithError(err)
			return
		}
	}
	if err := flushPendingEvent(); err != nil {
		_ = destination.CloseWithError(err)
		return
	}
	if err := scanner.Err(); err != nil {
		_ = destination.CloseWithError(err)
		return
	}
	_ = destination.Close()
}

func outputItemsText(items []responsesOutputItem) string {
	var text strings.Builder
	for _, item := range items {
		text.WriteString(outputItemText(item))
	}
	return text.String()
}

func outputItemText(item responsesOutputItem) string {
	if item.Type != "message" {
		return ""
	}
	var text strings.Builder
	for _, part := range item.Content {
		if part.Type == "output_text" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func (proxy *responsesCompatibilityProxy) Stop() {
	if proxy == nil || proxy.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = proxy.server.Shutdown(ctx)
	cancel()
	select {
	case <-proxy.done:
	case <-time.After(2 * time.Second):
	}
}
