package oauthproxy

import (
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

func TestLoginCopilotUsesGitHubDeviceFlowAndStoresDistinctCredential(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/login/device/code":
			if request.FormValue("client_id") != copilotOAuthClientID || request.FormValue("scope") != copilotOAuthScope {
				t.Fatalf("device form = %#v", request.Form)
			}
			writeTestJSON(writer, map[string]any{
				"device_code": "device", "user_code": "ABCD-EFGH",
				"verification_uri": serverURL(request) + "/device", "expires_in": 60, "interval": 0,
			})
		case "/login/oauth/access_token":
			polls.Add(1)
			writeTestJSON(writer, map[string]any{"access_token": "github-secret", "token_type": "bearer", "scope": "read:user"})
		case "/user":
			if request.Header.Get("Authorization") != "Bearer github-secret" {
				t.Fatalf("user authorization = %q", request.Header.Get("Authorization"))
			}
			writeTestJSON(writer, map[string]any{"login": "octocat", "name": "The Octocat", "id": 1})
		case "/models":
			if request.Header.Get("Authorization") != "Bearer github-secret" {
				t.Fatalf("models authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("X-Request-Id") != "" {
				t.Fatalf("unexpected X-Request-Id = %q", request.Header.Get("X-Request-Id"))
			}
			writeTestJSON(writer, copilotTestModels())
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	previousBase, previousGitHubAPI, previousCopilotAPI, previousFloor := copilotGitHubBaseURL, copilotGitHubAPIBaseURL, copilotAPIBaseURL, copilotOAuthPollFloor
	copilotGitHubBaseURL, copilotGitHubAPIBaseURL, copilotAPIBaseURL, copilotOAuthPollFloor = server.URL, server.URL, server.URL, time.Millisecond
	t.Cleanup(func() {
		copilotGitHubBaseURL, copilotGitHubAPIBaseURL, copilotAPIBaseURL, copilotOAuthPollFloor = previousBase, previousGitHubAPI, previousCopilotAPI, previousFloor
	})

	authDir := t.TempDir()
	result, err := loginCopilot(context.Background(), authDir, LoginOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != ProviderCopilot || result.Provider != ProviderCopilot {
		t.Fatalf("login result = %+v", result)
	}
	if filepath.Base(result.Path) != "copilot-octocat.json" || polls.Load() != 1 {
		t.Fatalf("path=%q polls=%d", result.Path, polls.Load())
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["type"] != ProviderCopilot || stored["github_token"] != "github-secret" || stored["login"] != "octocat" {
		t.Fatalf("stored credential = %#v", stored)
	}
	if info, err := os.Stat(result.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode: info=%v err=%v", info, err)
	}
}

func TestValidateCopilotEntitlementRejectsGitHubOnlyAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"Copilot subscription required"}`))
	}))
	defer server.Close()
	previousCopilotAPI := copilotAPIBaseURL
	copilotAPIBaseURL = server.URL
	t.Cleanup(func() { copilotAPIBaseURL = previousCopilotAPI })
	err := validateCopilotEntitlement(context.Background(), server.Client(), "github-token")
	if err == nil || !strings.Contains(err.Error(), "GitHub login succeeded but Copilot is unavailable") {
		t.Fatalf("entitlement error = %v", err)
	}
}

func TestCopilotGatewayFallsBackToExchangedToken(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/copilot_internal/v2/token" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer github-token" {
			t.Fatalf("exchange authorization = %q", request.Header.Get("Authorization"))
		}
		writeTestJSON(writer, map[string]any{"token": "copilot-token", "expires_at": time.Now().Add(time.Hour).Unix()})
	}))
	defer github.Close()

	var directRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Request-Id") != "" {
			t.Fatalf("unexpected X-Request-Id = %q", request.Header.Get("X-Request-Id"))
		}
		if request.Header.Get("Authorization") == "Bearer github-token" {
			if request.Header.Get("Editor-Version") != "" {
				t.Fatalf("direct GitHub token unexpectedly has Editor-Version = %q", request.Header.Get("Editor-Version"))
			}
			directRequests.Add(1)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Authorization") != "Bearer copilot-token" {
			t.Fatalf("Copilot authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("Editor-Version") != copilotIDETokenEditorVersion {
			t.Fatalf("IDE token Editor-Version = %q", request.Header.Get("Editor-Version"))
		}
		if request.Header.Get("Openai-Intent") != "conversation-edits" && request.URL.Path != "/models" {
			t.Fatalf("Openai-Intent = %q", request.Header.Get("Openai-Intent"))
		}
		switch request.URL.Path {
		case "/models":
			writeTestJSON(writer, copilotTestModels())
		case "/chat/completions":
			writeTestJSON(writer, map[string]any{"ok": true})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()

	previousGitHubAPI, previousCopilotAPI := copilotGitHubAPIBaseURL, copilotAPIBaseURL
	copilotGitHubAPIBaseURL, copilotAPIBaseURL = github.URL, api.URL
	t.Cleanup(func() {
		copilotGitHubAPIBaseURL, copilotAPIBaseURL = previousGitHubAPI, previousCopilotAPI
	})

	authDir := t.TempDir()
	writeCopilotTestCredential(t, authDir, "copilot-test.json", "github-token")
	pool := newCopilotCredentialPool(authDir, nil, false, nil)
	gateway, err := startCopilotGateway(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	models, err := gateway.discoverModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || directRequests.Load() != 1 {
		t.Fatalf("models=%+v direct requests=%d", models, directRequests.Load())
	}
	response, err := gateway.do(context.Background(), http.MethodPost, "/chat/completions", "", http.Header{"Content-Type": {"application/json"}}, []byte(`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", response.StatusCode)
	}
}

func TestBuildCopilotRoutesChoosesAdvertisedEndpoint(t *testing.T) {
	models := filterCopilotModels(copilotTestModels().Data)
	routes, err := buildCopilotRoutes("chat-model,response-model,claude-model", models)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes.chat) != 1 || len(routes.responses) != 1 || len(routes.anthropic) != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	if got := strings.Join(routes.models, ","); got != "chat-model,response-model,claude-model" {
		t.Fatalf("authoritative models = %q", got)
	}
	if _, err := buildCopilotRoutes("missing-model", models); err == nil || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestBuildCopilotRoutesKeepsCatalogWhenAddingContextAlias(t *testing.T) {
	models := filterCopilotModels(copilotTestModels().Data)
	routes, err := buildCopilotRoutes("chat-model[1m]", models)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes.chat) != 2 || routes.chat[0].Alias != "chat-model" || routes.chat[1].Alias != "chat-model[1m]" {
		t.Fatalf("chat routes = %+v", routes.chat)
	}
	if len(routes.responses) != 1 || len(routes.anthropic) != 1 {
		t.Fatalf("configured model narrowed catalog: %+v", routes)
	}
}

func TestCopilotCredentialPoolListsButDoesNotUseDisabledCredentials(t *testing.T) {
	authDir := t.TempDir()
	writeCopilotTestCredential(t, authDir, "copilot-active.json", "active-token")
	disabled, err := json.Marshal(map[string]any{"type": ProviderCopilot, "disabled": true, "login": "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "copilot-disabled.json"), disabled, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newCopilotCredentialPool(authDir, nil, false, nil)
	ordered, err := pool.ordered()
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 1 || ordered[0].fileName != "copilot-active.json" {
		t.Fatalf("active credentials = %+v", ordered)
	}
	auths := pool.listAuths()
	if len(auths) != 2 || !auths[1].Disabled || auths[1].Status != "disabled" {
		t.Fatalf("listed auths = %+v", auths)
	}
}

func TestStartCopilotRuntimeDiscoversAndPublishesModels(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer github-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/models":
			writeTestJSON(writer, copilotTestModels())
		case "/chat/completions":
			writeTestJSON(writer, map[string]any{
				"id": "chatcmpl_test", "object": "chat.completion", "model": "chat-model",
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		case "/responses":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"response-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		case "/v1/messages":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-model\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			_, _ = fmt.Fprint(writer, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = fmt.Fprint(writer, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
			_, _ = fmt.Fprint(writer, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = fmt.Fprint(writer, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = fmt.Fprint(writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()
	previousCopilotAPI := copilotAPIBaseURL
	copilotAPIBaseURL = api.URL
	t.Cleanup(func() { copilotAPIBaseURL = previousCopilotAPI })

	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopilotTestCredential(t, authDir, "copilot-test.json", "github-token")
	runtime, err := startCopilotOAuthWithFiles(context.Background(), "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := strings.Join(runtime.Models(), ","); got != "chat-model,response-model,claude-model" {
		t.Fatalf("runtime models = %q", got)
	}
	req, err := http.NewRequest(http.MethodGet, runtime.Endpoint()+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("models status=%d body=%s", response.StatusCode, body)
	}
	for _, model := range []string{"chat-model", "response-model", "claude-model"} {
		if !strings.Contains(string(body), model) {
			t.Fatalf("published models missing %q: %s", model, body)
		}
	}
	for _, model := range runtime.Models() {
		payload := strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hi","max_output_tokens":1,"store":false}`, model))
		request, err := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/responses", payload)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("Responses probe model=%q status=%d body=%s", model, response.StatusCode, responseBody)
		}
	}
	if auths := runtime.ListAuths(); len(auths) != 1 || auths[0].Provider != ProviderCopilot {
		t.Fatalf("runtime auths = %+v", auths)
	}
}

func copilotTestModels() copilotModelsResponse {
	return copilotModelsResponse{Data: []copilotModel{
		{ID: "chat-model", SupportedEndpoints: []string{"/chat/completions"}},
		{ID: "response-model", SupportedEndpoints: []string{"/responses"}},
		{ID: "claude-model", SupportedEndpoints: []string{"/v1/messages"}},
	}}
}

func writeCopilotTestCredential(t *testing.T, authDir, name, token string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": ProviderCopilot, "github_token": token, "login": "octocat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
