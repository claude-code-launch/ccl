package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeKimiUpstreamModel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"kimi-k3", "k3"},
		{"kimi-k3[1m]", "k3"},
		{"kimi-k3[1m](1024)", "k3(1024)"},
		{"kimi-k2.7-code", "kimi-for-coding"},
		{"kimi-k2.7-code[1m]", "kimi-for-coding"},
		{"k2.7-code", "kimi-for-coding"},
		{"kimi-k2.7-code-highspeed", "kimi-for-coding-highspeed"},
		{"kimi-for-coding", "kimi-for-coding"},
		{"for-coding", "kimi-for-coding"},
		{"kimi-k3(high)", "k3(high)"},
	}
	for _, tc := range cases {
		if got := normalizeKimiUpstreamModel(tc.in); got != tc.want {
			t.Errorf("normalizeKimiUpstreamModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeKimiBodyLinksToolCallsAndDropsEmptyAssistant(t *testing.T) {
	in := `{
		"model":"kimi-for-coding",
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","content":"done"},
			{"role":"assistant","content":""}
		]
	}`
	out, err := normalizeKimiBody([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2 (empty assistant dropped): %s", len(messages), out)
	}
	first := messages[0].(map[string]any)
	if _, ok := first["reasoning_content"]; !ok {
		t.Fatalf("assistant missing back-filled reasoning_content: %+v", first)
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %+v", tool)
	}
}

func TestNormalizeKimiBodyPatchesCallIDToToolCallID(t *testing.T) {
	in := `{
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","call_id":"call_9","content":"result"}
		]
	}`
	out, err := normalizeKimiBody([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	tool := messages[1].(map[string]any)
	if tool["tool_call_id"] != "call_9" {
		t.Fatalf("tool_call_id not patched from call_id: %+v", tool)
	}
}

func TestNormalizeKimiBodyNoopWhenNothingToPatch(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"hi"}]}`
	out, err := normalizeKimiBody([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte(in)) {
		t.Fatalf("expected no-op body, got %s", out)
	}
}

func TestKimiOAuthRefreshesOnlyAfter401(t *testing.T) {
	var upstreamCalls atomic.Int32
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("client_id") != kimiOAuthClientID || request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "refresh-old" {
				t.Errorf("refresh form = %+v", request.Form)
			}
			if request.Header.Get("X-Msh-Platform") != "CLIProxyAPI" || request.Header.Get("X-Msh-Device-Id") != "device-1" {
				t.Errorf("refresh headers = %+v", request.Header)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`)
		case "/coding/v1/chat/completions":

			upstreamCalls.Add(1)
			if request.Header.Get("X-Msh-Platform") != "CLIProxyAPI" || request.Header.Get("X-Msh-Device-Id") != "device-1" || request.Header.Get("X-Msh-Device-Name") == "" {
				t.Errorf("chat identity headers = %+v", request.Header)
			}
			if request.Header.Get("Authorization") == "Bearer access-old" {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(writer, `{"error":{"message":"expired"}}`)
				return
			}
			if request.Header.Get("Authorization") != "Bearer access-new" {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["model"] != "kimi-for-coding" {
				t.Errorf("upstream model = %v", payload["model"])
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"index\":0,\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello kimi\"},\"index\":0,\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
			_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	previousBase, previousToken := kimiAPIBaseURL, kimiTokenURL
	kimiAPIBaseURL, kimiTokenURL = server.URL+"/coding/v1", server.URL+"/token"
	t.Cleanup(func() { kimiAPIBaseURL, kimiTokenURL = previousBase, previousToken })

	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(authDir, "kimi-refresh.json")
	credential := []byte(`{"type":"kimi","access_token":"access-old","refresh_token":"refresh-old","device_id":"device-1","expired":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)
	if err := os.WriteFile(credentialPath, credential, 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := StartOAuth(context.Background(), ProviderKimi, "kimi-for-coding", filepath.Base(credentialPath))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := strings.Join(runtime.Models(), ","); got != "kimi-for-coding" {
		t.Fatalf("runtime models = %q", got)
	}
	body := postClaudeMessage(t, context.Background(), runtime, "kimi-for-coding")
	if !strings.Contains(body, "hello kimi") || !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("Messages response = %s", body)
	}
	if upstreamCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d refresh calls=%d", upstreamCalls.Load(), refreshCalls.Load())
	}
	stored, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`"access_token": "access-new"`)) || !bytes.Contains(stored, []byte(`"refresh_token": "refresh-new"`)) {
		t.Fatalf("refreshed credential was not persisted: %s", stored)
	}
}
