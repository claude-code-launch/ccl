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
			if request.Header.Get("X-IDE-Type") != workbuddyChatChannel || request.Header.Get("X-IDE-Name") != workbuddyChatChannel || request.Header.Get("X-IDE-Version") != workbuddyClientVersion || request.Header.Get("X-Private-Data") != "true" || request.Header.Get("X-Agent-Purpose") != "conversation" || request.Header.Get("x-requested-with") != "XMLHttpRequest" || request.Header.Get("X-Conversation-ID") == "" || request.Header.Get("X-Request-ID") == "" {
				t.Fatalf("chat identity headers = %+v", request.Header)
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
	if !strings.Contains(responseBody, "hello from WorkBuddy") || !strings.Contains(responseBody, `"type":"message_stop"`) {
		t.Fatalf("Messages response = %s", responseBody)
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
		refreshStatus int
		wantRefreshes int32
	}{
		{name: "rate limit is not retried", status: http.StatusTooManyRequests, wantRefreshes: 0},
		{name: "failed auth refresh keeps original forbidden", status: http.StatusForbidden, refreshStatus: http.StatusInternalServerError, wantRefreshes: 1},
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
					_, _ = writer.Write([]byte(`{"error":{"message":"provider-owned"}}`))
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
			if response.StatusCode != test.status || !strings.Contains(string(body), "provider-owned") || response.Header.Get("Retry-After") != "7" {
				t.Fatalf("response status=%d retry=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), body)
			}
			if chats.Load() != 1 || refreshes.Load() != test.wantRefreshes {
				t.Fatalf("chats=%d refreshes=%d", chats.Load(), refreshes.Load())
			}
		})
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
		payload := []byte(`{"model":"wb-chat","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		request, err := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", bytes.NewReader(payload))
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
		{ID: "fast-model", Name: "快速"},
		{ID: "balanced-model", Name: "均衡"},
		{ID: "deep-model"},
	}
	routes, _, _, _ := buildWorkBuddyRoutes("", catalog)
	for _, want := range []runtimeModelRoute{
		{Name: "fast-model", Alias: "快速"},
		{Name: "balanced-model", Alias: "均衡"},
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
