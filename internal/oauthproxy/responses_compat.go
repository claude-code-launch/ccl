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

// responsesCompatibilityProxy leaves Responses traffic untouched except for a
// CLIProxyAPI edge case: some upstreams emit the full answer only in the final
// response.completed event. CLIProxyAPI's streaming Claude translator currently
// ignores that text, so expose it as a standard output_text delta first.
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

func startResponsesCompatibilityProxy(targetEndpoint string, identity codexRequestIdentity) (*responsesCompatibilityProxy, error) {
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
			_ = normalizeCodexRequestIdentity(request.Out, identity)
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
		case "session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata":
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
	seenText := false

	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			var event responsesEvent
			if json.Unmarshal([]byte(payload), &event) == nil {
				switch event.Type {
				case "response.created":
					seenCreated = true
				case "response.output_text.delta":
					if event.Delta != "" {
						seenText = true
					}
				case "response.output_item.done":
					if outputItemText(event.Item) != "" {
						seenText = true
					}
				case "response.completed", "response.incomplete":
					if !seenCreated {
						created, _ := json.Marshal(map[string]any{
							"type": "response.created",
							"response": map[string]any{
								"id":    event.Response.ID,
								"model": event.Response.Model,
							},
						})
						if _, err := fmt.Fprintf(destination, "data: %s\n\n", created); err != nil {
							_ = destination.CloseWithError(err)
							return
						}
						seenCreated = true
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
				}
			}
		}
		if _, err := destination.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			_ = destination.CloseWithError(err)
			return
		}
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
