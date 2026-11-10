package oauthproxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// loopbackFrontMux serves Responses and adapts Chat Completions to it.
func loopbackFrontMux(key string, models, responses http.HandlerFunc) *http.ServeMux {
	chat := chatCompletionsFront(key, responses)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	for _, prefix := range []string{"/v1", ""} {
		mux.HandleFunc(prefix+"/models", models)
		mux.HandleFunc(prefix+"/responses", responses)
		mux.HandleFunc(prefix+"/responses/compact", responses)
		mux.HandleFunc(prefix+"/chat/completions", chat)
	}
	return mux
}

// loopbackAuthorized checks the per-session key a client sends as a bearer
// token (or x-api-key). The comparison is constant-time.
func loopbackAuthorized(request *http.Request, key string) bool {
	if key == "" {
		return false
	}
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
// writing the error. It reports whether the handler may proceed.
func modelsRequestAllowed(writer http.ResponseWriter, request *http.Request, key string) bool {
	if !loopbackAuthorized(request, key) {
		writeProxyError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return false
	}
	if request.Method != http.MethodGet {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return false
	}
	return true
}

// inferenceRequestAllowed applies the same checks to a POST inference request.
func inferenceRequestAllowed(writer http.ResponseWriter, request *http.Request, key string) bool {
	if !loopbackAuthorized(request, key) {
		writeProxyError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return false
	}
	if request.Method != http.MethodPost {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return false
	}
	return true
}

// modelListEntry is one row of the OpenAI-shaped model list. api_backend tells
// clients that support several wire protocols (such as Grok Build) to use
// Responses, the protocol every CCL runtime implements without translation on
// the client side.
func modelListEntry(model string) map[string]any {
	return map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "ccl", "api_backend": "responses"}
}

// serveModelList answers a model-list request with plain IDs.
func serveModelList(writer http.ResponseWriter, request *http.Request, key string, models []string) {
	if !modelsRequestAllowed(writer, request, key) {
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, modelListEntry(model))
	}
	writeModelList(writer, data)
}

func writeModelList(writer http.ResponseWriter, data []map[string]any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
}

// writeProxyError writes an OpenAI-shaped error body.
func writeProxyError(writer http.ResponseWriter, status int, errorType, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errorType, "code": nil, "param": nil},
	})
}

// prepareEventStream sets the headers of a server-sent event response.
func prepareEventStream(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
}
