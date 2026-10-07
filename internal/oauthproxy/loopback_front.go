package oauthproxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Every loopback runtime serves the same Anthropic-shaped front to Claude
// Code: a health check, the model list, Messages, and token counting, each
// with and without the /v1 prefix, behind the per-session key. These helpers
// are that front; a backend supplies only its handlers.

// anthropicFrontMux routes the shared paths to a backend's handlers.
func anthropicFrontMux(models, messages, countTokens http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	for _, prefix := range []string{"/v1", ""} {
		mux.HandleFunc(prefix+"/models", models)
		mux.HandleFunc(prefix+"/messages", messages)
		mux.HandleFunc(prefix+"/messages/count_tokens", countTokens)
	}
	return mux
}

// loopbackAuthorized checks the per-session key Claude Code sends as x-api-key
// or a bearer token. The comparison is constant-time.
func loopbackAuthorized(request *http.Request, key string) bool {
	if keyMatches(request.Header.Get("x-api-key"), key) {
		return true
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	return keyMatches(bearer, key)
}

func keyMatches(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// modelsRequestAllowed rejects an unauthorized or non-GET model-list request,
// writing the Anthropic error. It reports whether the handler may proceed.
func modelsRequestAllowed(writer http.ResponseWriter, request *http.Request, key string) bool {
	if !loopbackAuthorized(request, key) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return false
	}
	if request.Method != http.MethodGet {
		writeAnthropicError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return false
	}
	return true
}

// serveModelList answers a model-list request with plain IDs in Anthropic's
// paginated shape.
func serveModelList(writer http.ResponseWriter, request *http.Request, key string, models []string) {
	if !modelsRequestAllowed(writer, request, key) {
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{"id": model, "object": "model", "type": "model"})
	}
	first, last := modelPageBounds(models)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"object": "list", "data": data, "has_more": false, "first_id": first, "last_id": last,
	})
}
