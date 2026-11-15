package oauthproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	responsesHistoryMaxEntries   = 64
	responsesHistoryMaxTotalSize = 64 << 20
	responsesHistoryMaxEntrySize = 16 << 20
	responsesHistoryMaxPeekBytes = int64(128 << 20)
	responsesHistoryTTL          = 30 * time.Minute
)

type responsesHistoryEntry struct {
	id        string
	itemsJSON []byte
	expiresAt time.Time
}

// responsesHistoryCache stores recent Responses turns in memory per loopback
// runtime so clients using stateful `previous_response_id` (such as Grok Build
// or OpenAI Responses SDKs) work seamlessly against stateless or converted
// providers without persisting conversation state upstream (`store=false`).
type responsesHistoryCache struct {
	mu        sync.Mutex
	entries   map[string]responsesHistoryEntry
	order     []string
	totalSize int
}

func newResponsesHistoryCache() *responsesHistoryCache {
	return &responsesHistoryCache{
		entries: make(map[string]responsesHistoryEntry),
	}
}

func (c *responsesHistoryCache) put(id string, items []any, now time.Time) {
	id = strings.TrimSpace(id)
	if id == "" || len(items) == 0 {
		return
	}
	encoded, err := json.Marshal(items)
	if err != nil || len(encoded) == 0 || len(encoded) > responsesHistoryMaxEntrySize {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if existing, ok := c.entries[id]; ok {
		c.totalSize -= len(existing.itemsJSON)
		c.removeOrderLocked(id)
	}
	for (len(c.order) >= responsesHistoryMaxEntries || c.totalSize+len(encoded) > responsesHistoryMaxTotalSize) && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if victim, ok := c.entries[oldest]; ok {
			c.totalSize -= len(victim.itemsJSON)
			delete(c.entries, oldest)
		}
	}
	c.entries[id] = responsesHistoryEntry{
		id:        id,
		itemsJSON: encoded,
		expiresAt: now.Add(responsesHistoryTTL),
	}
	c.order = append(c.order, id)
	c.totalSize += len(encoded)
}

func (c *responsesHistoryCache) get(id string, now time.Time) ([]any, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	entry, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	entry.expiresAt = now.Add(responsesHistoryTTL)
	c.entries[id] = entry
	c.removeOrderLocked(id)
	c.order = append(c.order, id)

	var items []any
	if err := decodeProtocolJSON(entry.itemsJSON, &items); err != nil {
		return nil, false
	}
	return items, true
}

func (c *responsesHistoryCache) pruneLocked(now time.Time) {
	if len(c.order) == 0 {
		return
	}
	kept := c.order[:0]
	for _, id := range c.order {
		entry, ok := c.entries[id]
		if !ok {
			continue
		}
		if now.After(entry.expiresAt) {
			c.totalSize -= len(entry.itemsJSON)
			delete(c.entries, id)
			continue
		}
		kept = append(kept, id)
	}
	c.order = kept
}

func (c *responsesHistoryCache) removeOrderLocked(target string) {
	for i, id := range c.order {
		if id == target {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func withResponsesHistory(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	cache := newResponsesHistoryCache()
	return func(writer http.ResponseWriter, request *http.Request) {
		if !loopbackAuthorized(request, apiKey) || request.Method != http.MethodPost || request.Body == nil {
			next(writer, request)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, responsesHistoryMaxPeekBytes+1))
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if int64(len(raw)) > responsesHistoryMaxPeekBytes {
			request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), request.Body))
			next(writer, request)
			return
		}

		requestBytes := raw
		if prev := gjson.GetBytes(raw, "previous_response_id"); prev.Exists() && prev.Type != gjson.Null {
			prevID := strings.TrimSpace(prev.String())
			if prev.Type != gjson.String {
				writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "previous_response_id must be a string")
				return
			}
			if prevID != "" {
				priorItems, ok := cache.get(prevID, time.Now())
				if !ok {
					writeProxyErrorWithCode(writer, http.StatusBadRequest, "invalid_request_error", "previous_response_not_found",
						"previous_response_id is unknown or expired")
					return
				}
				var payload map[string]any
				if err := decodeProtocolJSON(raw, &payload); err != nil {
					writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request JSON")
					return
				}
				currentItems, err := normalizeResponsesInputSlice(payload["input"])
				if err != nil {
					writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
					return
				}
				combined := make([]any, 0, len(priorItems)+len(currentItems))
				combined = append(combined, priorItems...)
				combined = append(combined, currentItems...)
				payload["input"] = combined
				delete(payload, "previous_response_id")
				expanded, err := json.Marshal(payload)
				if err != nil {
					writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "encode expanded Responses request")
					return
				}
				requestBytes = expanded
			} else {
				var payload map[string]any
				if decodeProtocolJSON(raw, &payload) == nil {
					delete(payload, "previous_response_id")
					if cleaned, err := json.Marshal(payload); err == nil {
						requestBytes = cleaned
					}
				}
			}
		}

		request.Body = io.NopCloser(bytes.NewReader(requestBytes))
		request.ContentLength = int64(len(requestBytes))
		observer := &responsesHistoryObserver{target: writer}
		next(observer, request)
		if responseID, outputItems, ok := observer.completedResponse(); ok {
			inputItems := extractResponsesInputForCache(requestBytes)
			history := make([]any, 0, len(inputItems)+len(outputItems))
			history = append(history, inputItems...)
			history = append(history, outputItems...)
			cache.put(responseID, history, time.Now())
		}
	}
}

func normalizeResponsesInputSlice(raw any) ([]any, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "message", "role": "user", "content": value}}, nil
	case []any:
		return value, nil
	default:
		return nil, errInvalidResponsesInput
	}
}

var errInvalidResponsesInput = &proxyInputError{"Responses input must be a string or item array"}

type proxyInputError struct {
	msg string
}

func (e *proxyInputError) Error() string { return e.msg }

func extractResponsesInputForCache(raw []byte) []any {
	var payload map[string]any
	if decodeProtocolJSON(raw, &payload) != nil {
		return nil
	}
	items, _ := normalizeResponsesInputSlice(payload["input"])
	return items
}

func writeProxyErrorWithCode(writer http.ResponseWriter, status int, errorType, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	var codeValue any
	if code != "" {
		codeValue = code
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errorType, "code": codeValue, "param": nil},
	})
}

type responsesHistoryObserver struct {
	target        http.ResponseWriter
	status        int
	wroteHeader   bool
	streaming     bool
	overflow      bool
	body          bytes.Buffer
	pendingSSE    []byte
	completedBody []byte
}

func (o *responsesHistoryObserver) Header() http.Header {
	return o.target.Header()
}

func (o *responsesHistoryObserver) Unwrap() http.ResponseWriter {
	return o.target
}

func (o *responsesHistoryObserver) Flush() {
	if flusher, ok := o.target.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (o *responsesHistoryObserver) WriteHeader(statusCode int) {
	if !o.wroteHeader {
		o.status = statusCode
		o.wroteHeader = true
		o.streaming = strings.Contains(strings.ToLower(o.target.Header().Get("Content-Type")), "text/event-stream")
	}
	o.target.WriteHeader(statusCode)
}

func (o *responsesHistoryObserver) Write(p []byte) (int, error) {
	if !o.wroteHeader {
		o.WriteHeader(http.StatusOK)
	}
	n, err := o.target.Write(p)
	if n > 0 && o.status == http.StatusOK && !o.overflow {
		chunk := p[:n]
		if o.streaming {
			o.observeSSE(chunk)
		} else if o.body.Len()+len(chunk) <= responsesHistoryMaxEntrySize {
			_, _ = o.body.Write(chunk)
		} else {
			o.body.Reset()
			o.overflow = true
		}
	}
	return n, err
}

func (o *responsesHistoryObserver) observeSSE(chunk []byte) {
	o.pendingSSE = append(o.pendingSSE, chunk...)
	for {
		idx := bytes.Index(o.pendingSSE, []byte("\n\n"))
		if idx < 0 {
			break
		}
		o.inspectSSEFrame(o.pendingSSE[:idx])
		o.pendingSSE = o.pendingSSE[idx+2:]
	}
	if len(o.pendingSSE) > responsesHistoryMaxEntrySize {
		o.pendingSSE = nil
		o.overflow = true
	}
}

func (o *responsesHistoryObserver) inspectSSEFrame(frame []byte) {
	if !bytes.Contains(frame, []byte("response.completed")) && !bytes.Contains(frame, []byte("response.incomplete")) {
		return
	}
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if len(data) == 0 || len(data) > responsesHistoryMaxEntrySize {
		return
	}
	kind := gjson.GetBytes(data, "type").String()
	if kind != "response.completed" && kind != "response.incomplete" {
		return
	}
	if resp := gjson.GetBytes(data, "response"); resp.Exists() && resp.IsObject() {
		o.completedBody = append(o.completedBody[:0], []byte(resp.Raw)...)
	}
}

func (o *responsesHistoryObserver) completedResponse() (string, []any, bool) {
	if o.status != http.StatusOK || o.overflow {
		return "", nil, false
	}
	if o.streaming && len(o.pendingSSE) > 0 {
		o.inspectSSEFrame(o.pendingSSE)
		o.pendingSSE = nil
	}
	raw := o.completedBody
	if !o.streaming {
		raw = o.body.Bytes()
	}
	if len(raw) == 0 {
		return "", nil, false
	}
	var response map[string]any
	if decodeProtocolJSON(raw, &response) != nil {
		return "", nil, false
	}
	id := strings.TrimSpace(stringValue(response["id"]))
	if id == "" {
		return "", nil, false
	}
	output, _ := response["output"].([]any)
	return id, output, true
}
