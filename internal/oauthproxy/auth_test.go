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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/codexidentity"
	log "github.com/sirupsen/logrus"
)

func TestBackendProviderAliases(t *testing.T) {
	tests := map[string]string{
		"codex":     "codex",
		"gpt":       "codex",
		"chatgpt":   "codex",
		"copilot":   "copilot",
		"qoder":     "qoder",
		"gemini":    "antigravity",
		"grok":      "xai",
		"xai":       "xai",
		"kimi":      "kimi",
		"kiro":      "kiro",
		"claude":    "claude",
		"workbuddy": "workbuddy",
	}
	for input, want := range tests {
		got, err := BackendProvider(input)
		if err != nil || got != want {
			t.Fatalf("BackendProvider(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := BackendProvider("unknown"); err == nil {
		t.Fatal("BackendProvider(unknown) should fail")
	}
}

func TestValidateLoginProviderAcceptsPublicNames(t *testing.T) {
	for _, name := range []string{ProviderChatGPT, ProviderGemini, ProviderGrok, ProviderCopilot, ProviderQoder, ProviderKimi, ProviderKiro, ProviderClaude, ProviderWorkBuddy} {
		if _, err := ValidateLoginProvider(name); err != nil {
			t.Fatalf("ValidateLoginProvider(%q) error: %v", name, err)
		}
	}
	if got, err := ValidateLoginProvider(ProviderChatGPTLegacy); err != nil || got != ProviderChatGPT {
		t.Fatalf("ValidateLoginProvider(chatgpt legacy) = %q, %v; want %q", got, err, ProviderChatGPT)
	}
	for _, name := range []string{ProviderCodex, "antigravity", "xai", ""} {
		if _, err := ValidateLoginProvider(name); err == nil {
			t.Fatalf("ValidateLoginProvider(%q) should fail", name)
		}
	}
}

func TestKiroCredentialPoolFiltersExactCredential(t *testing.T) {
	authDir := t.TempDir()
	credentials := map[string][]byte{
		"kiro-a.json":  []byte(`{"type":"kiro","access_token":"a","refresh_token":"ra","client_id":"ca","client_secret":"sa"}`),
		"kiro-b.json":  []byte(`{"type":"kiro","access_token":"b","refresh_token":"rb","client_id":"cb","client_secret":"sb"}`),
		"codex-c.json": []byte(`{"type":"codex","access_token":"c"}`),
	}
	for name, data := range credentials {
		if err := os.WriteFile(filepath.Join(authDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	pool := newKiroCredentialPool(authDir, "kiro-b.json")
	auths, err := pool.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || filepath.Base(auths[0].fileName) != "kiro-b.json" {
		t.Fatalf("filtered Kiro auths = %+v", auths)
	}
}

func TestKiroRuntimeModelsIncludeConfiguredRoutes(t *testing.T) {
	models := kiroRuntimeModels("claude-sonnet-4-6[1m]")
	got := make(map[string]bool, len(models))
	for _, model := range models {
		got[model] = true
	}
	for _, want := range []string{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6"} {
		if !got[want] {
			t.Fatalf("Kiro models missing %q: %+v", want, models)
		}
	}
	if got["claude-opus-4-6"] || got["claude-haiku-4-5"] {
		t.Fatalf("Kiro models contain unconfigured static defaults: %+v", models)
	}
}

func TestNormalizeOpenAIBaseURLDoesNotRewriteUserBasePath(t *testing.T) {
	tests := map[string]string{
		"https://new.sharedchat.cc/codex":              "https://new.sharedchat.cc/codex",
		"https://new.sharedchat.cc/codex/v1":           "https://new.sharedchat.cc/codex/v1",
		"https://new.sharedchat.cc/codex/v1/responses": "https://new.sharedchat.cc/codex/v1",
		"https://api.openai.com/v1":                    "https://api.openai.com/v1",
		"https://example.com/api/v1/responses":         "https://example.com/api/v1",
	}
	for input, want := range tests {
		if got := normalizeOpenAIBaseURL(input); got != want {
			t.Errorf("normalizeOpenAIBaseURL(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestValidateLoginProviderHidesCodexBackend(t *testing.T) {
	for _, target := range []string{ProviderChatGPT, ProviderGemini} {
		got, err := ValidateLoginProvider(target)
		if err != nil || got != target {
			t.Fatalf("ValidateLoginProvider(%q) = %q, %v", target, got, err)
		}
	}
	if _, err := ValidateLoginProvider(ProviderCodex); err == nil {
		t.Fatal("Codex backend should not be exposed as a login provider")
	}
	if backend, err := BackendProvider(ProviderCodex); err != nil || backend != ProviderCodex {
		t.Fatalf("legacy Codex runtime mapping = %q, %v", backend, err)
	}
}

func TestEnsureAuthDirSecuresExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	if err := os.Chmod(authDir, 0o755); err != nil {
		t.Fatalf("set permissive auth dir mode: %v", err)
	}

	got, err := ensureAuthDir()
	if err != nil {
		t.Fatalf("ensureAuthDir() error: %v", err)
	}
	if got != authDir {
		t.Fatalf("ensureAuthDir() = %q, want %q", got, authDir)
	}
	info, err := os.Stat(authDir)
	if err != nil {
		t.Fatalf("stat auth dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("auth dir mode = %o, want 700", mode)
	}
}

func TestProviderTokenStoreFiltersOtherBackends(t *testing.T) {
	authDir := t.TempDir()
	credentials := map[string][]byte{
		"codex.json":       []byte(`{"type":"codex","access_token":"codex-token","email":"codex@example.com"}`),
		"antigravity.json": []byte(`{"type":"antigravity","access_token":"gemini-token","project_id":"test-project","email":"gemini@example.com"}`),
	}
	for name, data := range credentials {
		if err := os.WriteFile(filepath.Join(authDir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	store := newProviderTokenStore(authDir, ProviderCodex, "codex.json")
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 || auths[0].Provider != ProviderCodex {
		t.Fatalf("filtered auths = %+v, want one Codex auth", auths)
	}
}

func TestProviderTokenStoreFiltersByCredentialFile(t *testing.T) {
	authDir := t.TempDir()
	credentials := map[string][]byte{
		"codex-alice@example.com.json": []byte(`{"type":"codex","access_token":"alice","email":"alice@example.com"}`),
		"codex-bob@example.com.json":   []byte(`{"type":"codex","access_token":"bob","email":"bob@example.com"}`),
		"xai-ada@example.com.json":     []byte(`{"type":"xai","access_token":"ada","email":"ada@example.com"}`),
	}
	for name, data := range credentials {
		if err := os.WriteFile(filepath.Join(authDir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Bound to one credential file: only that account loads.
	store := newProviderTokenStore(authDir, ProviderCodex, "codex-bob@example.com.json")
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("filtered List() error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("credential-bound auths = %d, want 1", len(auths))
	}
	if got := filepath.Base(auths[0].FileName); got != "codex-bob@example.com.json" {
		t.Fatalf("selected file = %q, want codex-bob@example.com.json", got)
	}
}

func TestStartEmbeddedProxyWithStoredCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credential := []byte(`{"type":"codex","access_token":"test-token","refresh_token":"test-refresh","email":"test@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "codex-test.json"), credential, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOAuth(ctx, ProviderCodex, "", "codex-test.json")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxyRuntime.Stop()
	if proxyRuntime.APIKey() == "" {
		t.Fatal("Start() returned an empty session API key")
	}
	if proxyRuntime.coreManager != nil || proxyRuntime.httpServer == nil {
		t.Fatal("GPT subscription must use the CCL-owned HTTP runtime, not CPA")
	}

	unauthorizedResp, err := http.Get(proxyRuntime.Endpoint() + "/models")
	if err != nil {
		t.Fatalf("unauthorized models request: %v", err)
	}
	_ = unauthorizedResp.Body.Close()
	if unauthorizedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized models status = %d, want 401", unauthorizedResp.StatusCode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyRuntime.Endpoint()+"/models", nil)
	if err != nil {
		t.Fatalf("create models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200", resp.StatusCode)
	}

	endpoint := proxyRuntime.Endpoint()
	proxyRuntime.Stop()
	if _, err := http.Get(endpoint + "/models"); err == nil {
		t.Fatal("Codex runtime still accepts connections after Stop()")
	}
}

func TestDirectCodexRuntimeDoesNotChangeSDKLogger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credential := []byte(`{"type":"codex","access_token":"test-token","refresh_token":"test-refresh","email":"test@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "codex-log-test.json"), credential, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	originalOut := log.StandardLogger().Out
	originalLevel := log.GetLevel()
	var output bytes.Buffer
	log.SetOutput(&output)
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		log.SetOutput(originalOut)
		log.SetLevel(originalLevel)
	})

	proxyRuntime, err := StartOAuth(context.Background(), ProviderCodex, "", "codex-log-test.json")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	log.Warn("hidden while embedded runtime is active")
	if !strings.Contains(output.String(), "hidden while embedded runtime is active") {
		proxyRuntime.Stop()
		t.Fatalf("direct runtime unexpectedly changed the process logger: %q", output.String())
	}

	output.Reset()
	proxyRuntime.Stop()
	log.Warn("still hidden after embedded runtime stops")
	if !strings.Contains(output.String(), "still hidden after embedded runtime stops") {
		t.Fatalf("direct runtime did not preserve the process logger after stop: %q", output.String())
	}
}

func TestStartOpenAIResponsesAPIUsesCCLOwnedCodexIdentity(t *testing.T) {
	type capture struct {
		header http.Header
		body   map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-5.4-mini\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1/responses", "upstream-key", "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	payload := []byte(`{"model":"gpt-5.4-mini","input":"hi","stream":true,"max_output_tokens":8,"metadata":{"source":"claude"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyRuntime.Endpoint()+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("responses request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}

	got := <-captured
	if got.header.Get("Authorization") != "Bearer upstream-key" {
		t.Fatalf("upstream authorization = %q", got.header.Get("Authorization"))
	}
	if !strings.HasPrefix(strings.ToLower(got.header.Get("User-Agent")), "codex_cli_rs/") {
		t.Fatalf("Responses gateway did not receive CCL's Codex User-Agent: %q", got.header.Get("User-Agent"))
	}
	if got.header.Get("Originator") != codexidentity.Originator {
		t.Fatalf("Responses gateway Originator = %q, want %s", got.header.Get("Originator"), codexidentity.Originator)
	}
	if got.header.Get("Version") != codexidentity.ClientVersion {
		t.Fatalf("Responses gateway Version = %q, want %s", got.header.Get("Version"), codexidentity.ClientVersion)
	}
	if got.header.Get("X-Codex-Beta-Features") != "" {
		t.Fatalf("generic Responses gateway received X-Codex-Beta-Features = %q", got.header.Get("X-Codex-Beta-Features"))
	}
	sessionID := got.header.Get("Session-Id")
	if sessionID == "" || got.header.Get("Thread-Id") != sessionID || got.header.Get("X-Client-Request-Id") != sessionID {
		t.Fatalf("Responses turn identity is inconsistent: headers=%v", got.header)
	}
	if got.header.Get("Session_id") != "" {
		t.Fatalf("legacy Session_id unexpectedly present: headers=%v", got.header)
	}
	metadata, _ := got.body["client_metadata"].(map[string]any)
	if metadata["session_id"] != sessionID || metadata["thread_id"] != sessionID ||
		metadata["x-codex-window-id"] != got.header.Get("X-Codex-Window-Id") || metadata["x-codex-installation-id"] == "" {
		t.Fatalf("Responses client_metadata does not match headers: headers=%v body=%v", got.header, got.body)
	}
	if stream, _ := got.body["stream"].(bool); !stream {
		t.Fatalf("Responses request did not force streaming: %+v", got.body)
	}
	if got.body["model"] != "gpt-5.4-mini" {
		t.Fatalf("upstream model = %v, want gpt-5.4-mini", got.body["model"])
	}

	if proxyRuntime.coreManager != nil || proxyRuntime.httpServer == nil {
		t.Fatal("Responses API key runtime unexpectedly depends on CPA")
	}
}

func TestStartOpenAIChatAPIServesClaudeMessages(t *testing.T) {
	type capture struct {
		path   string
		header http.Header
		body   map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"chat ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIChatAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test,gpt-test[1m]")
	if err != nil {
		t.Fatalf("StartOpenAIChatAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test[1m]")
	if !strings.Contains(responseBody, "chat ok") || !strings.Contains(responseBody, `"type":"message_stop"`) {
		t.Fatalf("CLIProxyAPI did not return Claude SSE: %s", responseBody)
	}
	got := <-captured
	if got.path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got.path)
	}
	if got.header.Get("Authorization") != "Bearer upstream-key" {
		t.Fatalf("upstream authorization = %q", got.header.Get("Authorization"))
	}
	if got.body["model"] != "gpt-test" {
		t.Fatalf("upstream model = %v, want gpt-test", got.body["model"])
	}
}

func TestStartOpenAIResponsesAPIServesClaudeMessages(t *testing.T) {
	type capture struct {
		path string
		body map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"in_progress\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"responses ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"responses ok\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"responses ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test,gpt-test[1m]")
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()
	models := runtimeModelIDs(t, ctx, proxyRuntime)
	if !models["gpt-test"] || !models["gpt-test[1m]"] {
		t.Fatalf("CLIProxyAPI models = %v, want base model and 1M alias", models)
	}

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test[1m]")
	if !strings.Contains(responseBody, "responses ok") || !strings.Contains(responseBody, `"type":"message_stop"`) {
		t.Fatalf("CLIProxyAPI did not return Claude SSE: %s", responseBody)
	}
	if count := strings.Count(responseBody, "responses ok"); count != 1 {
		t.Fatalf("CLIProxyAPI returned Responses text %d times, want once: %s", count, responseBody)
	}
	got := <-captured
	if got.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got.path)
	}
	if got.body["model"] != "gpt-test" {
		t.Fatalf("upstream model = %v, want gpt-test", got.body["model"])
	}
}

func runtimeModelIDs(t *testing.T, ctx context.Context, proxyRuntime *Runtime) map[string]bool {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyRuntime.Endpoint()+"/models", nil)
	if err != nil {
		t.Fatalf("create models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("models status = %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	models := make(map[string]bool, len(payload.Data))
	for _, model := range payload.Data {
		models[model.ID] = true
	}
	return models
}

func postClaudeMessage(t *testing.T, ctx context.Context, proxyRuntime *Runtime, model string) string {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyRuntime.Endpoint()+"/messages", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create Claude request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Claude request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read Claude response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Claude response status = %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

func TestStopClosesDirectCodexRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credential := []byte(`{"type":"codex","access_token":"test-token","refresh_token":"test-refresh","email":"test@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "codex-cleanup.json"), credential, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOAuth(ctx, ProviderCodex, "", "codex-cleanup.json")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	auths := proxyRuntime.ListAuths()
	if len(auths) != 1 {
		proxyRuntime.Stop()
		t.Fatalf("runtime auth count = %d, want 1", len(auths))
	}
	if proxyRuntime.coreManager != nil || proxyRuntime.httpServer == nil {
		proxyRuntime.Stop()
		t.Fatal("Codex subscription unexpectedly started a CPA runtime")
	}

	endpoint := proxyRuntime.Endpoint()
	proxyRuntime.Stop()
	if _, err := http.Get(endpoint + "/models"); err == nil {
		t.Fatal("direct Codex runtime still accepts connections after Stop()")
	}
}

func TestStartEmbeddedProxyExposesOnlyRequestedProviderModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	credentials := map[string][]byte{
		"codex.json":       []byte(`{"type":"codex","access_token":"codex-token","refresh_token":"codex-refresh","email":"codex@example.com"}`),
		"antigravity.json": []byte(`{"type":"antigravity","access_token":"gemini-token","refresh_token":"gemini-refresh","project_id":"test-project","email":"gemini@example.com"}`),
	}
	for name, data := range credentials {
		if err := os.WriteFile(filepath.Join(authDir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOAuth(ctx, ProviderCodex, "", "codex.json")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxyRuntime.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyRuntime.Endpoint()+"/models", nil)
	if err != nil {
		t.Fatalf("create models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if len(payload.Data) == 0 {
		t.Fatal("Codex runtime returned no models")
	}
	for _, model := range payload.Data {
		if strings.HasPrefix(strings.ToLower(model.ID), "gemini-") {
			t.Fatalf("Codex runtime exposed Gemini model %q", model.ID)
		}
	}
}

func TestStartRequiresMatchingCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := StartOAuth(context.Background(), ProviderGemini, "", "antigravity-missing.json")
	if err == nil {
		t.Fatal("Start() should fail without Gemini credentials")
	}
}

func TestSilenceStdoutNestedReferenceCount(t *testing.T) {
	original := os.Stdout
	t.Cleanup(func() {
		os.Stdout = original
		stdoutState.Lock()
		if stdoutState.sink != nil {
			_ = stdoutState.sink.Close()
		}
		stdoutState.users = 0
		stdoutState.original = nil
		stdoutState.sink = nil
		stdoutState.Unlock()
	})

	restoreOuter := silenceStdout()
	if os.Stdout == original {
		t.Fatal("outer silenceStdout should redirect os.Stdout")
	}
	redirected := os.Stdout

	restoreInner := silenceStdout()
	if os.Stdout != redirected {
		t.Fatal("nested silenceStdout should reuse the same sink")
	}

	restoreInner()
	if os.Stdout != redirected {
		t.Fatal("inner restore must keep stdout silenced while outer is active")
	}

	restoreOuter()
	if os.Stdout != original {
		t.Fatal("outer restore should put back the original stdout")
	}
}

func TestStartOpenAIResponsesAPIUsesCCLDataPlane(t *testing.T) {
	type capture struct {
		path   string
		header http.Header
		body   map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"in_progress\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"plain ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"plain ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test")
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test")
	if !strings.Contains(responseBody, "plain ok") {
		t.Fatalf("plain Responses runtime did not return Claude SSE: %s", responseBody)
	}

	got := <-captured
	if got.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got.path)
	}
	if got.header.Get("Authorization") != "Bearer upstream-key" {
		t.Fatalf("upstream authorization = %q", got.header.Get("Authorization"))
	}
	if got.body["model"] != "gpt-test" {
		t.Fatalf("upstream model = %v, want gpt-test", got.body["model"])
	}
	if proxyRuntime.coreManager != nil || proxyRuntime.service != nil || proxyRuntime.httpServer == nil {
		t.Fatal("Responses request was not served by CCL's direct data plane")
	}
}

func TestAPIKeyResponsesPreservesRetryAfterWithoutGlobalCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test")
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	payload := []byte(`{"model":"gpt-test","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxyRuntime.Endpoint()+"/messages", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "37" {
		t.Fatalf("response = HTTP %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestStartProviderResponsesUsesCCLOwnedCodexIdentity(t *testing.T) {
	type capture struct {
		header http.Header
		body   map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capture{header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"in_progress\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"routed ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"routed ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartProvider(ctx, StartOptions{
		Protocol:  ProtocolOpenAIResponses,
		Endpoint:  upstream.URL + "/v1",
		APIKey:    "upstream-key",
		ModelSpec: "gpt-test",
	})
	if err != nil {
		t.Fatalf("StartProvider(plain responses) error: %v", err)
	}
	defer proxyRuntime.Stop()

	_ = postClaudeMessage(t, ctx, proxyRuntime, "gpt-test")
	got := <-captured
	if got.header.Get("Originator") != codexidentity.Originator || got.header.Get("Version") != codexidentity.ClientVersion ||
		!strings.HasPrefix(got.header.Get("User-Agent"), codexidentity.Originator+"/") {
		t.Fatalf("StartProvider Responses missing CCL Codex identity: headers=%v body=%v", got.header, got.body)
	}
}

func TestStartOpenAIChatAPIToolCall(t *testing.T) {
	type capture struct {
		body map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_tool\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"weather\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_tool\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIChatAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test")
	if err != nil {
		t.Fatalf("StartOpenAIChatAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessageWithTools(t, ctx, proxyRuntime, "gpt-test")
	assertClaudeToolUse(t, responseBody, "lookup", "weather")
	got := <-captured
	tools, _ := got.body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("upstream chat request missing tools: %+v", got.body)
	}
}

func TestStartOpenAIResponsesAPIToolCall(t *testing.T) {
	type capture struct {
		path string
		body map[string]any
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capture{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tool\",\"model\":\"gpt-test\",\"status\":\"in_progress\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"status\":\"in_progress\",\"arguments\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\\\"weather\\\"}\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"arguments\":\"{\\\"q\\\":\\\"weather\\\"}\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"weather\\\"}\",\"status\":\"completed\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"weather\\\"}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test")
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessageWithTools(t, ctx, proxyRuntime, "gpt-test")
	assertClaudeToolUse(t, responseBody, "lookup", "weather")
	got := <-captured
	if got.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got.path)
	}
}

func postClaudeMessageWithTools(t *testing.T, ctx context.Context, proxyRuntime *Runtime, model string) string {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{
		"model":%q,
		"max_tokens":64,
		"stream":true,
		"tools":[{"name":"lookup","description":"look something up","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"use lookup"}]
	}`, model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyRuntime.Endpoint()+"/messages", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create Claude tool request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+proxyRuntime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Claude tool request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read Claude tool response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Claude tool response status = %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

func assertClaudeToolUse(t *testing.T, responseBody, toolName, argSnippet string) {
	t.Helper()

	var (
		sawToolUseType bool
		sawToolName    bool
		sawArgSnippet  bool
		stopReason     string
	)

	for line := range strings.SplitSeq(responseBody, "\n") {
		line = strings.TrimSpace(line)
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "content_block_start":
			block, _ := event["content_block"].(map[string]any)
			if block == nil {
				continue
			}
			if blockType, _ := block["type"].(string); blockType == "tool_use" {
				sawToolUseType = true
			}
			if name, _ := block["name"].(string); name == toolName {
				sawToolName = true
			}
		case "content_block_delta":
			delta, _ := event["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if partial, _ := delta["partial_json"].(string); argSnippet != "" && strings.Contains(partial, argSnippet) {
				sawArgSnippet = true
			}
		case "message_delta":
			delta, _ := event["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if reason, _ := delta["stop_reason"].(string); reason != "" {
				stopReason = reason
			}
		case "message_start":
			message, _ := event["message"].(map[string]any)
			if message == nil {
				continue
			}
			if reason, _ := message["stop_reason"].(string); reason != "" {
				stopReason = reason
			}
		}
	}

	if !sawToolUseType {
		t.Fatalf("missing content_block_start tool_use: %s", responseBody)
	}
	if !sawToolName {
		t.Fatalf("missing tool name %q in content_block_start: %s", toolName, responseBody)
	}
	if argSnippet != "" && !sawArgSnippet {
		// Some translators emit the full JSON only in content_block_start.input.
		if !strings.Contains(responseBody, argSnippet) {
			t.Fatalf("missing tool arg %q: %s", argSnippet, responseBody)
		}
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use: %s", stopReason, responseBody)
	}
}

func TestStartProviderTreatsCodexV1AsOrdinaryResponsesBase(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	runtime, err := StartProvider(context.Background(), StartOptions{
		Protocol:  ProtocolOpenAIResponses,
		Endpoint:  upstream.URL + "/codex/v1",
		APIKey:    "test-key",
		ModelSpec: "gpt-5.4-mini",
	})
	if err != nil {
		t.Fatalf("StartProvider() rejected ordinary base path: %v", err)
	}
	runtime.Stop()
}
