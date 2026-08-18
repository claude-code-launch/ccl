package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeGeminiCredential(t *testing.T, home, name string) string {
	t.Helper()
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(authDir, name)
	credential := []byte(`{"type":"antigravity","access_token":"access-old","refresh_token":"refresh-old","email":"test@example.com","project_id":"test-project","expired":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)
	if err := os.WriteFile(path, credential, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGeminiOAuthRefreshFormEncoding(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", request.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(request.Body)
		gotForm, _ = url.ParseQuery(string(body))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"access-new","expires_in":3600}`)
	}))
	defer server.Close()

	previous := antigravityTokenURL
	antigravityTokenURL = server.URL
	t.Cleanup(func() { antigravityTokenURL = previous })

	home := t.TempDir()
	path := writeGeminiCredential(t, home, "gemini-refresh.json")
	authorizer := &antigravityOAuthAuthorizer{
		path:   path,
		client: &http.Client{},
	}
	token, err := authorizer.authorize(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-new" {
		t.Errorf("token = %q, want access-new", token)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("client_id") != antigravityOAuthClientID ||
		gotForm.Get("client_secret") != antigravityOAuthClientSecret || gotForm.Get("refresh_token") != "refresh-old" {
		t.Errorf("refresh form = %+v", gotForm)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`"access_token": "access-new"`)) {
		t.Errorf("refreshed credential not persisted: %s", stored)
	}
}

func TestGeminiRuntimeStreamsThrough(t *testing.T) {
	var upstreamCalls atomic.Int32
	var gotAuthorization atomic.Value
	var gotEnvelope atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		if !strings.Contains(request.URL.Path, "streamGenerateContent") {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt = %q", request.URL.Query().Get("alt"))
		}
		gotAuthorization.Store(request.Header.Get("Authorization"))
		if request.Header.Get("User-Agent") != antigravityRequestUserAgent {
			t.Errorf("user agent = %q", request.Header.Get("User-Agent"))
		}
		body, _ := io.ReadAll(request.Body)
		gotEnvelope.Store(string(body))
		if request.Header.Get("Authorization") == "Bearer access-old" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":{"message":"expired"}}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello from gemini\"}]}}]}}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"thoughtsTokenCount\":0}}}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"access-new","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	previousDaily, previousProd, previousToken := antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL
	antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL = server.URL, server.URL, tokenServer.URL
	t.Cleanup(func() {
		antigravityBaseURLDaily, antigravityBaseURLProd, antigravityTokenURL = previousDaily, previousProd, previousToken
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	credPath := writeGeminiCredential(t, home, "gemini-e2e.json")

	runtime, err := StartOAuth(context.Background(), ProviderGemini, "gemini-3-flash", filepath.Base(credPath))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := strings.Join(runtime.Models(), ","); got != "gemini-3-flash" {
		t.Fatalf("runtime models = %q", got)
	}

	body := postClaudeMessage(t, context.Background(), runtime, "gemini-3-flash")
	if !strings.Contains(body, "message_start") || !strings.Contains(body, "hello from gemini") || !strings.Contains(body, "message_stop") {
		t.Fatalf("Messages response = %s", body)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (401 then retry)", upstreamCalls.Load())
	}
	if got := gotAuthorization.Load().(string); got != "Bearer access-new" {
		t.Errorf("final authorization = %q", got)
	}
	envelope := gotEnvelope.Load().(string)
	var parsed struct {
		Project string `json:"project"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("envelope JSON: %v", err)
	}
	if parsed.Project != "test-project" || parsed.Model != "gemini-3-flash" {
		t.Errorf("envelope project/model = %q/%q", parsed.Project, parsed.Model)
	}
}
