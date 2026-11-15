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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginWorkBuddyPollsAndPersistsBoundCredential(t *testing.T) {
	var tokenPolls atomic.Int32
	var accountPolls atomic.Int32
	var openedURL string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v2/plugin/auth/state":
			if request.Method != http.MethodPost || request.URL.Query().Get("platform") != workbuddyPlatform {
				t.Fatalf("state request = %s %s", request.Method, request.URL.String())
			}
			if request.Header.Get("X-No-Authorization") != "true" || request.Header.Get("X-Product") != "SaaS" {
				t.Fatalf("state headers = %+v", request.Header)
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"state": "fresh-state", "authUrl": serverURL(request) + "/login/?platform=workbuddy-ai&state=fresh-state",
			}})
		case "/v2/plugin/auth/token":
			if request.URL.Query().Get("state") != "fresh-state" {
				t.Fatalf("token state = %q", request.URL.Query().Get("state"))
			}
			if tokenPolls.Add(1) == 1 {
				writeTestJSON(writer, map[string]any{"code": workbuddyPendingToken, "msg": "pending"})
				return
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": "access-one", "refreshToken": "refresh-one", "expiresIn": 3600,
				"refreshExpiresIn": 7200, "domain": request.Host,
			}})
		case "/v2/plugin/login/account":
			if request.Header.Get("Authorization") != "Bearer access-one" || request.Header.Get("X-Domain") != request.Host {
				t.Fatalf("account headers = %+v", request.Header)
			}
			if accountPolls.Add(1) == 1 {
				writeTestJSON(writer, map[string]any{"code": workbuddyPendingAccount, "msg": "pending"})
				return
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"uid": "user-123", "nickname": "initial",
			}})
		case "/v2/plugin/accounts":
			if request.Header.Get("Authorization") != "Bearer access-one" {
				t.Fatalf("accounts authorization = %q", request.Header.Get("Authorization"))
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"accounts": []map[string]any{{
				"uid": "user-123", "nickname": "Alice", "email": "alice@example.com", "type": "Personal",
			}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	restoreWorkBuddyTestGlobals(t, server.URL)
	workbuddyBrowserOpener = func(target string) error {
		openedURL = target
		return nil
	}
	workbuddyPollInterval = time.Millisecond
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := loginWorkBuddy(context.Background(), authDir, ProviderWorkBuddy, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != ProviderWorkBuddy || result.Backend != ProviderWorkBuddy {
		t.Fatalf("login result = %+v", result)
	}
	if !strings.Contains(openedURL, "state=fresh-state") || !strings.Contains(openedURL, "version="+workbuddyClientVersion) {
		t.Fatalf("opened URL = %q", openedURL)
	}
	if tokenPolls.Load() != 2 || accountPolls.Load() != 2 {
		t.Fatalf("polls token=%d account=%d", tokenPolls.Load(), accountPolls.Load())
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["type"] != ProviderWorkBuddy || metadata["user_id"] != "user-123" || metadata["nickname"] != "Alice" {
		t.Fatalf("credential metadata = %+v", metadata)
	}
	if firstMetadataString(metadata, "access_token") != "access-one" || metadataInt64(metadata, "expires_at") <= time.Now().UnixMilli() {
		t.Fatalf("credential token metadata = %+v", metadata)
	}
}

func TestWorkBuddyRuntimeUsesCCLChatAdapterAndRefreshesOnce(t *testing.T) {
	var chatAttempts atomic.Int32
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v3/config":
			if request.Header.Get("Authorization") != "Bearer old-token" || request.Header.Get("X-User-Id") != "user-1" {
				t.Fatalf("config headers = %+v", request.Header)
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"models": []map[string]any{{
				"id": "wb-chat", "name": "WorkBuddy Chat", "supportsToolCall": true, "supportsReasoning": true,
				"credits": "x0.29",
			}}}})
		case "/v2/plugin/auth/token/refresh":
			refreshes.Add(1)
			if request.Header.Get("Authorization") != "Bearer old-token" || request.Header.Get("X-Refresh-Token") != "refresh-token" {
				t.Fatalf("refresh headers = %+v", request.Header)
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": "new-token", "refreshToken": "refresh-token", "expiresIn": 3600, "domain": request.Host,
			}})
		case "/v2/chat/completions":
			attempt := chatAttempts.Add(1)
			if request.Header.Get("X-IDE-Type") != workbuddyChatChannel || request.Header.Get("X-IDE-Name") != workbuddyChatChannel || request.Header.Get("X-IDE-Version") != workbuddyClientVersion || request.Header.Get("X-Private-Data") != "false" || request.Header.Get("X-Agent-Type") != "main" || request.Header.Get("X-Agent-Purpose") != "conversation" || request.Header.Get("x-requested-with") != "XMLHttpRequest" || request.Header.Get("X-Conversation-ID") == "" || request.Header.Get("X-Request-ID") == "" {
				t.Fatalf("chat identity headers = %+v", request.Header)
			}
			if !strings.Contains(request.Header.Get("User-Agent"), "WorkBuddy AI/"+workbuddyClientVersion) {
				t.Fatalf("international chat UA = %q", request.Header.Get("User-Agent"))
			}
			if attempt == 1 {
				if request.Header.Get("Authorization") != "Bearer old-token" {
					t.Fatalf("first authorization = %q", request.Header.Get("Authorization"))
				}
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"error":{"message":"expired"}}`))
				return
			}
			if request.Header.Get("Authorization") != "Bearer new-token" {
				t.Fatalf("second authorization = %q", request.Header.Get("Authorization"))
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"wb-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from WorkBuddy\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"wb-chat\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n")
			_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	restoreWorkBuddyTestGlobals(t, server.URL)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkBuddyTestCredential(t, authDir, "workbuddy-user-1.json", "old-token", "refresh-token", time.Now().Add(time.Hour))
	runtime, err := StartOAuth(context.Background(), ProviderWorkBuddy, "wb-chat[1m]", "workbuddy-user-1.json")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := strings.Join(runtime.Models(), ","); got != "wb-chat" {
		t.Fatalf("runtime models = %q", got)
	}
	if runtime.ModelDisplayNames()["wb-chat"] != "WorkBuddy Chat" {
		t.Fatalf("display names = %+v", runtime.ModelDisplayNames())
	}
	if details := runtime.ModelDetails(); details["wb-chat"].RateMultiplier == nil || *details["wb-chat"].RateMultiplier != 0.29 || details["wb-chat"].RateUnit != "credits" {
		t.Fatalf("model details = %+v", details)
	}
	responseBody := postClaudeMessage(t, context.Background(), runtime, "wb-chat[1m]")
	if !strings.Contains(responseBody, "hello from WorkBuddy") || !strings.Contains(responseBody, `"type":"response.completed"`) {
		t.Fatalf("Responses body = %s", responseBody)
	}
	if chatAttempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("attempts=%d refreshes=%d", chatAttempts.Load(), refreshes.Load())
	}
	if auths := runtime.ListAuths(); len(auths) != 1 || auths[0].Provider != ProviderWorkBuddy {
		t.Fatalf("runtime auths = %+v", auths)
	}
}

func TestWorkBuddyGatewayPreservesProviderErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		refreshStatus int
		wantRefreshes int32
	}{
		{name: "rate limit is not retried", status: http.StatusTooManyRequests, body: `{"error":{"message":"provider-owned"}}`, wantRefreshes: 0},
		{name: "failed auth refresh keeps original forbidden", status: http.StatusForbidden, body: `{"error":{"message":"provider-owned"}}`, refreshStatus: http.StatusInternalServerError, wantRefreshes: 1},
		{name: "restricted account is not refreshed", status: http.StatusForbidden, body: `{"code":11140,"msg":"request illegal"}`, wantRefreshes: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var chats atomic.Int32
			var refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v2/chat/completions":
					chats.Add(1)
					writer.Header().Set("Retry-After", "7")
					writer.WriteHeader(test.status)
					_, _ = writer.Write([]byte(test.body))
				case "/v2/plugin/auth/token/refresh":
					refreshes.Add(1)
					writer.WriteHeader(test.refreshStatus)
					_, _ = writer.Write([]byte(`{"code":5001,"msg":"refresh failed"}`))
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			restoreWorkBuddyTestGlobals(t, server.URL)

			authDir := t.TempDir()
			writeWorkBuddyTestCredential(t, authDir, "workbuddy-test.json", "token", "refresh", time.Now().Add(time.Hour))
			store := newWorkBuddyCredentialStore(workbuddyTestDeployment(t, ProviderWorkBuddy), authDir, "workbuddy-test.json")
			gateway, err := startWorkBuddyGateway(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Stop()
			request, err := http.NewRequest(http.MethodPost, gateway.endpoint+"/chat/completions", bytes.NewBufferString(`{"model":"wb-chat","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+gateway.key)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != test.status || !strings.Contains(string(body), test.body) || response.Header.Get("Retry-After") != "7" {
				t.Fatalf("response status=%d retry=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), body)
			}
			if chats.Load() != 1 || refreshes.Load() != test.wantRefreshes {
				t.Fatalf("chats=%d refreshes=%d", chats.Load(), refreshes.Load())
			}
		})
	}
}

func TestWorkBuddyDiscoverModelsDoesNotRefreshRestrictedAccount(t *testing.T) {
	var configs atomic.Int32
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v3/config":
			configs.Add(1)
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"code":11140,"msg":"request illegal"}`))
		case "/v2/plugin/auth/token/refresh":
			refreshes.Add(1)
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": "new-token", "refreshToken": "refresh-token", "expiresIn": 3600, "domain": request.Host,
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	restoreWorkBuddyTestGlobals(t, server.URL)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkBuddyTestCredential(t, authDir, "workbuddy-user-1.json", "old-token", "refresh-token", time.Now().Add(time.Hour))
	_, err := StartOAuth(context.Background(), ProviderWorkBuddy, "wb-chat", "workbuddy-user-1.json")
	if err == nil || !strings.Contains(err.Error(), "11140") {
		t.Fatalf("restricted catalog error = %v", err)
	}
	if configs.Load() != 1 || refreshes.Load() != 0 {
		t.Fatalf("configs=%d refreshes=%d", configs.Load(), refreshes.Load())
	}
}

// A provider Retry-After is relayed immediately by the outer retry loop.
// The gateway inner hop must not add an independent retry/cooldown layer.
func TestWorkBuddyRuntimePreservesProviderErrorsWithoutRetryOrCooldown(t *testing.T) {
	swapWorkBuddyFastRetry(t)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			testWorkBuddyRuntimeProviderError(t, status)
		})
	}
}

func testWorkBuddyRuntimeProviderError(t *testing.T, upstreamStatus int) {
	t.Helper()
	var chatAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v3/config":
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"models": []map[string]any{{
				"id": "wb-chat", "name": "WorkBuddy Chat", "supportsToolCall": true,
			}}}})
		case "/v2/chat/completions":
			chatAttempts.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Retry-After", "9")
			writer.WriteHeader(upstreamStatus)
			_, _ = writer.Write([]byte(`{"error":{"message":"workbuddy busy","type":"rate_limit_error"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	restoreWorkBuddyTestGlobals(t, server.URL)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkBuddyTestCredential(t, authDir, "workbuddy-user-1.json", "token", "refresh", time.Now().Add(time.Hour))
	runtime, err := StartOAuth(context.Background(), ProviderWorkBuddy, "wb-chat", "workbuddy-user-1.json")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	for attempt := 1; attempt <= 2; attempt++ {
		payload := []byte(`{"model":"wb-chat","max_output_tokens":32,"stream":true,"input":[{"role":"user","content":"hi"}]}`)
		request, err := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/responses", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != upstreamStatus || !strings.Contains(string(body), "workbuddy busy") || response.Header.Get("Retry-After") != "9" {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.StatusCode, body)
		}
	}
	if chatAttempts.Load() != 2 {
		t.Fatalf("chat attempts = %d, want one upstream call per client request", chatAttempts.Load())
	}
}

func workbuddyTestDeployment(t *testing.T, provider string) workbuddyDeployment {
	t.Helper()
	deployment, err := workbuddyDeploymentFor(provider)
	if err != nil {
		t.Fatal(err)
	}
	return deployment
}

func writeWorkBuddyTestCredential(t *testing.T, authDir, name, accessToken, refreshToken string, expiresAt time.Time) {
	t.Helper()
	writeWorkBuddyTestCredentialFor(t, ProviderWorkBuddy, authDir, name, accessToken, refreshToken, expiresAt)
}

func writeWorkBuddyTestCredentialFor(t *testing.T, provider, authDir, name, accessToken, refreshToken string, expiresAt time.Time) {
	t.Helper()
	deployment := workbuddyTestDeployment(t, provider)
	raw, err := json.Marshal(map[string]any{
		"type": deployment.provider, "access_token": accessToken, "refresh_token": refreshToken,
		"expires_at": expiresAt.UnixMilli(), "domain": deployment.domain(), "user_id": "user-1", "nickname": "Alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func restoreWorkBuddyTestGlobals(t *testing.T, baseURL string) {
	t.Helper()
	restoreWorkBuddyTestBaseURL(t, ProviderWorkBuddy, baseURL)
}

func restoreWorkBuddyTestBaseURL(t *testing.T, provider, baseURL string) {
	t.Helper()
	previousBaseURL, hadPreviousBaseURL := workbuddyBaseURLs[provider]
	previousBrowserOpener := workbuddyBrowserOpener
	previousPollInterval := workbuddyPollInterval
	previousLoginTimeout := workbuddyLoginTimeout
	previousHTTPTimeout := workbuddyHTTPTimeout
	workbuddyBaseURLs[provider] = baseURL
	t.Cleanup(func() {
		if hadPreviousBaseURL {
			workbuddyBaseURLs[provider] = previousBaseURL
		} else {
			delete(workbuddyBaseURLs, provider)
		}
		workbuddyBrowserOpener = previousBrowserOpener
		workbuddyPollInterval = previousPollInterval
		workbuddyLoginTimeout = previousLoginTimeout
		workbuddyHTTPTimeout = previousHTTPTimeout
	})
}

// swapWorkBuddyFastRetry shortens the shared fast-retry backoff for the two-hop
// runtime tests so retried requests do not sleep in real time. The gateway-level
// test above must NOT call this: it pins the inner hop at exactly one call.
func swapWorkBuddyFastRetry(t *testing.T) {
	t.Helper()
	previous := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() {
		upstreamFastRetryBackoff = previous
	})
}

func TestWorkBuddyUserAgentDiffersByDeployment(t *testing.T) {
	ai := workbuddyDeployment{provider: ProviderWorkBuddy, baseURL: workbuddyDefaultBaseURL}
	cn := workbuddyDeployment{provider: ProviderWorkBuddyCN, baseURL: workbuddyChinaDefaultBaseURL}
	wantAI := "WorkBuddy/" + workbuddyClientVersion + " WorkBuddy AI/" + workbuddyClientVersion + " CLI/" + workbuddyCLIVersion
	wantCN := "WorkBuddy/" + workbuddyClientVersion + " WorkBuddy/" + workbuddyClientVersion + " CLI/" + workbuddyCLIVersion
	if got := ai.userAgent(); got != wantAI {
		t.Fatalf("international UA = %q, want %q", got, wantAI)
	}
	if got := cn.userAgent(); got != wantCN {
		t.Fatalf("cn UA = %q, want %q", got, wantCN)
	}
}

// TestWorkBuddyChinaDeploymentIsSeparateAccountStore covers the second
// WorkBuddy deployment (www.workbuddy.cn) end to end: login talks to the .cn
// origin and types the credential workbuddycn, a credential minted by the
// other deployment is refused rather than sent to this origin, and the runtime
// serves the .cn account from that same origin.
func TestWorkBuddyChinaDeploymentIsSeparateAccountStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Domain") != request.Host {
			t.Fatalf("%s X-Domain = %q, want %q", request.URL.Path, request.Header.Get("X-Domain"), request.Host)
		}
		switch request.URL.Path {
		case "/v2/plugin/auth/state":
			if request.URL.Query().Get("platform") != workbuddyPlatform {
				t.Fatalf("state platform = %q", request.URL.Query().Get("platform"))
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"state": "cn-state", "authUrl": serverURL(request) + "/login?platform=workbuddy&state=cn-state",
			}})
		case "/v2/plugin/auth/token":
			if request.URL.Query().Get("state") != "cn-state" {
				t.Fatalf("token state = %q", request.URL.Query().Get("state"))
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": "cn-access", "refreshToken": "cn-refresh", "expiresIn": 3600, "domain": request.Host,
			}})
		case "/v2/plugin/login/account":
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"uid": "cn-user", "nickname": "China"}})
		case "/v2/plugin/accounts":
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"accounts": []map[string]any{{
				"uid": "cn-user", "nickname": "China", "email": "china@example.cn",
			}}}})
		case "/v3/config":
			if request.Header.Get("Authorization") != "Bearer cn-access" {
				t.Fatalf("config authorization = %q", request.Header.Get("Authorization"))
			}
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"models": []map[string]any{{
				"id": "cn-chat", "name": "WorkBuddy CN",
			}}}})
		case "/v2/chat/completions":
			if ua := request.Header.Get("User-Agent"); strings.Contains(ua, "WorkBuddy AI") || !strings.Contains(ua, "WorkBuddy/"+workbuddyClientVersion+" WorkBuddy/"+workbuddyClientVersion) {
				t.Fatalf("cn chat UA = %q", ua)
			}
			if request.Header.Get("X-Private-Data") != "false" || request.Header.Get("X-Agent-Type") != "main" {
				t.Fatalf("cn chat identity headers = %+v", request.Header)
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-cn\",\"object\":\"chat.completion.chunk\",\"model\":\"cn-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from workbuddy.cn\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-cn\",\"object\":\"chat.completion.chunk\",\"model\":\"cn-chat\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	restoreWorkBuddyTestBaseURL(t, ProviderWorkBuddyCN, server.URL)
	workbuddyBrowserOpener = func(string) error { return nil }
	workbuddyPollInterval = time.Millisecond

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := loginWorkBuddy(context.Background(), authDir, ProviderWorkBuddyCN, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != ProviderWorkBuddyCN || result.Backend != ProviderWorkBuddyCN {
		t.Fatalf("login result = %+v", result)
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["type"] != ProviderWorkBuddyCN || metadata["user_id"] != "cn-user" {
		t.Fatalf("cn credential metadata = %+v", metadata)
	}

	// The two deployments keep separate account stores: a .ai credential must
	// never have its token sent to the .cn origin.
	writeWorkBuddyTestCredential(t, authDir, "workbuddy-user-1.json", "ai-token", "ai-refresh", time.Now().Add(time.Hour))
	foreign := newWorkBuddyCredentialStore(workbuddyTestDeployment(t, ProviderWorkBuddyCN), authDir, "workbuddy-user-1.json")
	if _, err := foreign.load(); err == nil || !strings.Contains(err.Error(), ProviderWorkBuddyCN) {
		t.Fatalf("foreign credential error = %v", err)
	}

	runtime, err := StartOAuth(context.Background(), ProviderWorkBuddyCN, "cn-chat", filepath.Base(result.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if got := strings.Join(runtime.Models(), ","); got != "cn-chat" {
		t.Fatalf("runtime models = %q", got)
	}
	if body := postClaudeMessage(t, context.Background(), runtime, "cn-chat"); !strings.Contains(body, "hello from workbuddy.cn") {
		t.Fatalf("Messages response = %s", body)
	}
	if auths := runtime.ListAuths(); len(auths) != 1 || auths[0].Provider != ProviderWorkBuddyCN {
		t.Fatalf("runtime auths = %+v", auths)
	}
}

func TestBuildWorkBuddyRoutesSkipsModelsWithdrawnFromCatalog(t *testing.T) {
	catalog := []workbuddyModel{{ID: "kimi-k2.8"}, {ID: "glm-5"}}
	routes, models, _, _ := buildWorkBuddyRoutes("kimi-k2.7,GLM-5[1m]", catalog)
	if got := strings.Join(models, ","); got != "kimi-k2.8,glm-5" {
		t.Fatalf("models = %q", got)
	}
	for _, route := range routes {
		if strings.EqualFold(route.Name, "kimi-k2.7") || strings.EqualFold(route.Alias, "kimi-k2.7") {
			t.Fatalf("withdrawn model kept a route: %+v", routes)
		}
	}
	if !slices.Contains(routes, runtimeModelRoute{Name: "glm-5", Alias: "GLM-5[1m]"}) {
		t.Fatalf("catalog model lost its configured alias: %+v", routes)
	}
}

func TestBuildWorkBuddyRoutesMapsDisplayNames(t *testing.T) {
	catalog := []workbuddyModel{
		{ID: "fast-model", Name: "\u5feb\u901f"},
		{ID: "balanced-model", Name: "\u5747\u8861"},
		{ID: "deep-model"},
	}
	routes, _, _, _ := buildWorkBuddyRoutes("", catalog)
	for _, want := range []runtimeModelRoute{
		{Name: "fast-model", Alias: "\u5feb\u901f"},
		{Name: "balanced-model", Alias: "\u5747\u8861"},
	} {
		if !slices.Contains(routes, want) {
			t.Fatalf("missing display-name route %v in %+v", want, routes)
		}
	}
	if slices.ContainsFunc(routes, func(r runtimeModelRoute) bool {
		return strings.EqualFold(r.Alias, "deep-model") && r.Alias != "deep-model"
	}) {
		t.Fatalf("empty name produced an alias: %+v", routes)
	}
}

func TestBuildWorkBuddyRoutesKeepsFirstDisplayNameOwner(t *testing.T) {
	catalog := []workbuddyModel{
		{ID: "hy4-preview", Name: "Hy4 preview"},
		{ID: "hy4-preview-x", Name: "Hy4 preview"},
		{ID: "hy4-preview-exp", Name: "Hy4 preview Exp"},
	}
	routes, _, _, _ := buildWorkBuddyRoutes("", catalog)
	if !slices.Contains(routes, runtimeModelRoute{Name: "hy4-preview", Alias: "Hy4 preview"}) {
		t.Fatalf("canonical model lost its display-name alias: %+v", routes)
	}
	if slices.ContainsFunc(routes, func(r runtimeModelRoute) bool {
		return strings.EqualFold(r.Alias, "Hy4 preview") && !strings.EqualFold(r.Name, "hy4-preview")
	}) {
		t.Fatalf("later variant stole the shared display name: %+v", routes)
	}
	if !slices.Contains(routes, runtimeModelRoute{Name: "hy4-preview-x", Alias: "hy4-preview-x"}) {
		t.Fatalf("variant lost its technical id: %+v", routes)
	}
	if !slices.Contains(routes, runtimeModelRoute{Name: "hy4-preview-exp", Alias: "Hy4 preview Exp"}) {
		t.Fatalf("distinct display name was dropped: %+v", routes)
	}
}

func TestBuildWorkBuddyRoutesKeepsOnlyModelsWithCredits(t *testing.T) {
	catalog := []workbuddyModel{
		{ID: "fast-model", Name: "\u5feb\u901f", Credits: "x0.15"},
		{ID: "hy3", Name: "Hy3", Credits: "x0.00"},
		{ID: "codewise-jump"},
		{ID: "Kling-Video-V3", Name: "Kling-Video-V3"},
		{ID: "deep-model", Name: "\u6781\u81f4", Credits: "x1"},
	}
	routes, models, _, details := buildWorkBuddyRoutes("codewise-jump,fast-model", catalog)
	if got := strings.Join(models, ","); got != "fast-model,hy3,deep-model" {
		t.Fatalf("models = %q", got)
	}
	for _, route := range routes {
		if strings.EqualFold(route.Name, "codewise-jump") || strings.EqualFold(route.Alias, "codewise-jump") ||
			strings.EqualFold(route.Name, "Kling-Video-V3") {
			t.Fatalf("uncredited model kept a route: %+v", routes)
		}
	}
	if details["fast-model"].RateMultiplier == nil || *details["fast-model"].RateMultiplier != 0.15 {
		t.Fatalf("fast-model details = %+v", details["fast-model"])
	}
	if details["hy3"].RateMultiplier == nil || *details["hy3"].RateMultiplier != 0 {
		t.Fatalf("free model details = %+v", details["hy3"])
	}
}

func TestSanitizeWorkBuddyPayloadToolChoice(t *testing.T) {
	// 1. Object tool_choice normalized to string "auto" when tools are present.
	inputWithObj := []byte(`{
		"model": "wb-model",
		"tools": [{"type": "function", "function": {"name": "search"}}],
		"tool_choice": {"type": "function", "function": {"name": "search"}}
	}`)
	out, err := sanitizeWorkBuddyPayload(inputWithObj)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if tc, ok := res["tool_choice"].(string); !ok || tc != "auto" {
		t.Fatalf("expected tool_choice auto, got %+v", res["tool_choice"])
	}

	// 2. tool_choice="none" is preserved when tools are present.
	inputNone := []byte(`{
		"model": "wb-model",
		"tools": [{"type": "function", "function": {"name": "search"}}],
		"tool_choice": "none"
	}`)
	out, err = sanitizeWorkBuddyPayload(inputNone)
	if err != nil {
		t.Fatal(err)
	}
	res = make(map[string]any)
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if tc, ok := res["tool_choice"].(string); !ok || tc != "none" {
		t.Fatalf("expected tool_choice none, got %+v", res["tool_choice"])
	}

	// 3. tool_choice without tools is removed.
	inputNoTools := []byte(`{
		"model": "wb-model",
		"tool_choice": "auto"
	}`)
	out, err = sanitizeWorkBuddyPayload(inputNoTools)
	if err != nil {
		t.Fatal(err)
	}
	res = make(map[string]any)
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if _, exists := res["tool_choice"]; exists {
		t.Fatalf("expected tool_choice omitted when no tools, got %+v", res["tool_choice"])
	}
}

func TestSanitizeWorkBuddyPayloadSystemPromptsAndRoles(t *testing.T) {
	input := []byte(`{
		"model": "wb-model",
		"messages": [
			{
				"role": "developer",
				"content": "x-anthropic-billing-header: cc_version=2.1.37; cch=abc;\nYou are an AI agent powered by DeepSeek Harness.\nMain branch (you will usually use this for PRs)\nFollow instructions."
			},
			{
				"role": "system",
				"content": "x-anthropic-billing-hdr: cc_version=2.1.37; cch=def;"
			},
			{
				"role": "system",
				"content": "You are Claude Code, Anthropic's official CLI for Claude. Be helpful."
			},
			{
				"role": "system",
				"content": "You are Codex, a coding agent created by OpenAI."
			},
			{
				"role": "system",
				"content": "You are Grok released by xAI. You are an interactive CLI tool that helps users with software engineering tasks."
			},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hello "},
					{"type": "text", "text": "world"}
				]
			},
			{
				"role": "assistant",
				"content": [],
				"reasoning_content": "internal reasoning"
			},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "look at image"},
					{"type": "image_url", "image_url": {"url": "data:image/png;base64,123"}}
				]
			}
		]
	}`)
	out, err := sanitizeWorkBuddyPayload(input)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	msgs, ok := res["messages"].([]any)
	if !ok {
		t.Fatalf("expected messages array, got %+v", res["messages"])
	}
	// The 2nd system message (which only had billing header) should have been dropped.
	// So 8 input messages become 7 messages.
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages, got %d: %+v", len(msgs), msgs)
	}

	msg0 := msgs[0].(map[string]any)
	if msg0["role"] != "system" {
		t.Fatalf("developer role should be converted to system, got %v", msg0["role"])
	}
	content0 := msg0["content"].(string)
	if strings.Contains(content0, "x-anthropic-billing-header") {
		t.Fatalf("billing header not stripped from msg0: %s", content0)
	}
	if strings.Contains(content0, "DeepSeek Harness") {
		t.Fatalf("competitor identity not scrubbed from msg0: %s", content0)
	}
	if !strings.Contains(content0, "You are a helpful AI assistant") {
		t.Fatalf("expected helpful assistant in msg0: %s", content0)
	}
	if strings.Contains(content0, "usually use this for PRs") {
		t.Fatalf("PR fingerprint not cleaned from msg0: %s", content0)
	}
	if !strings.Contains(content0, "Main branch\nFollow instructions.") {
		t.Fatalf("expected preserved instruction in msg0: %s", content0)
	}

	msg1 := msgs[1].(map[string]any)
	content1 := msg1["content"].(string)
	if strings.Contains(content1, "Claude Code") {
		t.Fatalf("Claude Code identity not scrubbed: %s", content1)
	}
	if !strings.Contains(content1, "You are a helpful AI assistant. Be helpful.") {
		t.Fatalf("unexpected content1: %s", content1)
	}

	msg2 := msgs[2].(map[string]any)
	content2 := msg2["content"].(string)
	if strings.Contains(content2, "Codex") {
		t.Fatalf("Codex identity not scrubbed: %s", content2)
	}
	if content2 != "You are a helpful AI assistant." {
		t.Fatalf("unexpected content2: %s", content2)
	}

	msg3 := msgs[3].(map[string]any)
	content3 := msg3["content"].(string)
	if strings.Contains(content3, "Grok") || strings.Contains(content3, "xAI") {
		t.Fatalf("Grok/xAI identity not scrubbed: %s", content3)
	}
	if !strings.Contains(content3, "You are a helpful AI assistant. You are an interactive CLI tool") {
		t.Fatalf("unexpected content3: %s", content3)
	}

	msg4 := msgs[4].(map[string]any)
	if text, ok := msg4["content"].(string); !ok || text != "hello world" {
		t.Fatalf("expected flattened string 'hello world', got %+v", msg4["content"])
	}

	msg5 := msgs[5].(map[string]any)
	if _, exists := msg5["reasoning_content"]; exists {
		t.Fatalf("reasoning_content should be stripped, got %+v", msg5["reasoning_content"])
	}
	if text, ok := msg5["content"].(string); !ok || text != "" {
		t.Fatalf("expected empty string content for assistant, got %+v", msg5["content"])
	}

	msg6 := msgs[6].(map[string]any)
	if parts, ok := msg6["content"].([]any); !ok || len(parts) != 2 {
		t.Fatalf("multimodal message should keep parts array, got %+v", msg6["content"])
	}
}

func TestFilterWorkBuddyOutboundHeaders(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Anthropic-Billing-Header", "cc_version=2.1.37")
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Originator", "claude-code")
	headers.Set("X-Codex-Turn", "1")
	headers.Set("X-Stainless-Lang", "go")
	headers.Set("X-Opencode-Session", "sess-1")
	headers.Set("Session_id", "s-123")
	headers.Set("Cookie", "session=abc")
	headers.Set("Accept", "application/json")
	headers.Set("X-Custom-Safe", "value")

	filterWorkBuddyOutboundHeaders(headers)

	for _, stripped := range []string{
		"X-Anthropic-Billing-Header",
		"Anthropic-Version",
		"Originator",
		"X-Codex-Turn",
		"X-Stainless-Lang",
		"X-Opencode-Session",
		"Session_id",
		"Cookie",
	} {
		if headers.Get(stripped) != "" {
			t.Fatalf("header %s was not filtered", stripped)
		}
	}
	if headers.Get("Accept") != "application/json" {
		t.Fatalf("Accept header lost: %q", headers.Get("Accept"))
	}
	if headers.Get("X-Custom-Safe") != "value" {
		t.Fatalf("X-Custom-Safe header lost: %q", headers.Get("X-Custom-Safe"))
	}
}

func TestWorkBuddyRuntimeSanitizesOutboundUpstreamRequest(t *testing.T) {
	var receivedBody []byte
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v3/config":
			writeTestJSON(writer, map[string]any{"code": 0, "data": map[string]any{"models": []map[string]any{{
				"id": "wb-chat", "name": "WorkBuddy Chat", "supportsToolCall": true, "credits": "x0.20",
			}}}})
		case "/v2/chat/completions":
			receivedHeaders = request.Header.Clone()
			var err error
			receivedBody, err = io.ReadAll(request.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"wb-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"wb-chat\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	restoreWorkBuddyTestGlobals(t, server.URL)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkBuddyTestCredential(t, authDir, "workbuddy-user-sanitize.json", "token-1", "refresh-1", time.Now().Add(time.Hour))

	runtime, err := StartOAuth(context.Background(), ProviderWorkBuddy, "wb-chat", "workbuddy-user-sanitize.json")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()

	// Send a Responses request with:
	// - developer message containing x-anthropic-billing-header and DeepSeek Harness identity
	// - tool_choice specifying a function
	// - client headers like X-Anthropic-Billing-Header and Originator
	payload := []byte(`{
		"model": "wb-chat",
		"stream": true,
		"tools": [{"type": "function", "name": "search", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}],
		"tool_choice": {"type": "function", "name": "search"},
		"input": [
			{"role": "developer", "content": "x-anthropic-billing-header: cc_version=2.1.37; cch=xyz;\nYou are an AI agent powered by DeepSeek Harness.\nMain branch (you will usually use this for PRs)\nFind files."},
			{"role": "user", "content": "look up tests"}
		]
	}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, runtime.Endpoint()+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Anthropic-Billing-Header", "cc_version=2.1.37")
	req.Header.Set("Originator", "claude-code")
	req.Header.Set("X-Codex-Turn", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Responses request returned HTTP %d: %s", resp.StatusCode, respBytes)
	}

	// 1. Verify outbound headers received by WorkBuddy mock server
	if receivedHeaders.Get("X-Anthropic-Billing-Header") != "" {
		t.Fatalf("X-Anthropic-Billing-Header leaked to upstream")
	}
	if receivedHeaders.Get("Originator") != "" {
		t.Fatalf("Originator leaked to upstream")
	}
	if receivedHeaders.Get("X-Codex-Turn") != "" {
		t.Fatalf("X-Codex-Turn leaked to upstream")
	}
	if receivedHeaders.Get("X-IDE-Version") != workbuddyClientVersion {
		t.Fatalf("X-IDE-Version = %q, expected %q", receivedHeaders.Get("X-IDE-Version"), workbuddyClientVersion)
	}

	// 2. Verify outbound body received by WorkBuddy mock server
	var parsedBody map[string]any
	if err := json.Unmarshal(receivedBody, &parsedBody); err != nil {
		t.Fatalf("decode received body: %v; body = %s", err, receivedBody)
	}

	// tool_choice must be a string ("auto"), not an object
	tc, ok := parsedBody["tool_choice"].(string)
	if !ok || tc != "auto" {
		t.Fatalf("upstream received tool_choice = %+v (type %T), expected string 'auto'", parsedBody["tool_choice"], parsedBody["tool_choice"])
	}

	// messages must have scrubbed system prompt and no competitor identity
	rawMsgs := parsedBody["messages"].([]any)
	if len(rawMsgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %+v", rawMsgs)
	}
	systemMsg := rawMsgs[0].(map[string]any)
	if systemMsg["role"] != "system" {
		t.Fatalf("developer role should be remapped to system, got %+v", systemMsg["role"])
	}
	systemText := systemMsg["content"].(string)
	if strings.Contains(systemText, "x-anthropic-billing-header") {
		t.Fatalf("x-anthropic-billing-header was not stripped from upstream body: %s", systemText)
	}
	if strings.Contains(systemText, "DeepSeek Harness") {
		t.Fatalf("DeepSeek Harness identity was not scrubbed: %s", systemText)
	}
	if strings.Contains(systemText, "usually use this for PRs") {
		t.Fatalf("PR branch fingerprint not cleaned: %s", systemText)
	}
	if !strings.Contains(systemText, "You are a helpful AI assistant") {
		t.Fatalf("expected neutral assistant identity: %s", systemText)
	}
	if !strings.Contains(systemText, "Find files.") {
		t.Fatalf("expected instruction to be preserved: %s", systemText)
	}

	// 3. Test that Grok Build system prompt is also sanitized end-to-end
	grokPayload := []byte(`{
		"model": "wb-chat",
		"stream": true,
		"input": [
			{"role": "developer", "content": "You are Grok released by xAI. You are an interactive CLI tool that helps users with software engineering tasks. Your main goal is to complete the user's request in the <user_query> tag."},
			{"role": "user", "content": "hi"}
		]
	}`)
	grokReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, runtime.Endpoint()+"/responses", bytes.NewReader(grokPayload))
	if err != nil {
		t.Fatal(err)
	}
	grokReq.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	grokReq.Header.Set("Content-Type", "application/json")
	grokResp, err := http.DefaultClient.Do(grokReq)
	if err != nil {
		t.Fatal(err)
	}
	defer grokResp.Body.Close()
	if grokResp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(grokResp.Body)
		t.Fatalf("Grok Responses request returned HTTP %d: %s", grokResp.StatusCode, respBytes)
	}

	if err := json.Unmarshal(receivedBody, &parsedBody); err != nil {
		t.Fatalf("decode received grok body: %v; body = %s", err, receivedBody)
	}
	rawMsgs = parsedBody["messages"].([]any)
	systemMsg = rawMsgs[0].(map[string]any)
	systemText = systemMsg["content"].(string)
	if strings.Contains(systemText, "Grok") || strings.Contains(systemText, "xAI") {
		t.Fatalf("Grok/xAI identity leaked to upstream: %s", systemText)
	}
	if !strings.Contains(systemText, "You are a helpful AI assistant. You are an interactive CLI tool") {
		t.Fatalf("unexpected sanitized Grok prompt: %s", systemText)
	}
}
