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

	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

func TestBackendProviderAliases(t *testing.T) {
	tests := map[string]string{
		"codex":   "codex",
		"chatgpt": "codex",
		"copilot": "codex",
		"gemini":  "antigravity",
		"grok":    "xai",
		"xai":     "xai",
		"kimi":    "kimi",
		"claude":  "claude",
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
	for _, name := range []string{ProviderChatGPT, ProviderGemini, ProviderGrok, ProviderCopilot, ProviderKimi, ProviderClaude} {
		if _, err := ValidateLoginProvider(name); err != nil {
			t.Fatalf("ValidateLoginProvider(%q) error: %v", name, err)
		}
	}
	for _, name := range []string{ProviderCodex, "antigravity", "xai", ""} {
		if _, err := ValidateLoginProvider(name); err == nil {
			t.Fatalf("ValidateLoginProvider(%q) should fail", name)
		}
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

func TestNewerCodexClientVersion(t *testing.T) {
	tests := []struct {
		candidate string
		baseline  string
		want      bool
	}{
		{candidate: "0.144.4", baseline: "0.144.3", want: true},
		{candidate: "0.145.0", baseline: "0.144.99", want: true},
		{candidate: "1.0.0", baseline: "0.999.999", want: true},
		{candidate: "0.144.3", baseline: "0.144.4", want: false},
		{candidate: "0.144.4", baseline: "0.144.4", want: false},
	}
	for _, test := range tests {
		if got := newerCodexClientVersion(test.candidate, test.baseline); got != test.want {
			t.Errorf("newerCodexClientVersion(%q, %q) = %t, want %t", test.candidate, test.baseline, got, test.want)
		}
	}
}

func TestTerminalUserAgentTokenFallsBackToUnknown(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM_PROGRAM_VERSION", "")
	t.Setenv("TERM", "")
	if got := terminalUserAgentToken(); got != "unknown" {
		t.Fatalf("terminalUserAgentToken() = %q, want unknown", got)
	}
}

func TestStartCodexAPIRejectsCodexV1Endpoint(t *testing.T) {
	_, err := StartCodexAPI(context.Background(), "https://new.sharedchat.cc/codex/v1", "test-key", "gpt-5.4-mini")
	if err == nil || !strings.Contains(err.Error(), "https://new.sharedchat.cc/codex") {
		t.Fatalf("StartCodexAPI() error = %v", err)
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

	store := newProviderTokenStore(authDir, ProviderCodex, "")
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

	// No credential file: all backend accounts load (multi-account round-robin pool).
	store := newProviderTokenStore(authDir, ProviderCodex, "")
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("unfiltered codex auths = %d, want 2", len(auths))
	}

	// Bound to one credential file: only that account loads.
	store = newProviderTokenStore(authDir, ProviderCodex, "codex-bob@example.com.json")
	auths, err = store.List(context.Background())
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
	proxyRuntime, err := Start(ctx, ProviderCodex)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxyRuntime.Stop()
	if proxyRuntime.APIKey() == "" {
		t.Fatal("Start() returned an empty session API key")
	}
	if info, err := os.Stat(proxyRuntime.configPath); err != nil {
		t.Fatalf("stat runtime config: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime config mode = %o, want 600", info.Mode().Perm())
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

	configPath := proxyRuntime.configPath
	proxyRuntime.Stop()
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("runtime config still exists after Stop(): %v", err)
	}
}

func TestEmbeddedProxyKeepsSDKLogsIsolatedAfterStop(t *testing.T) {
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

	proxyRuntime, err := Start(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	log.Warn("hidden while embedded runtime is active")
	if output.Len() != 0 {
		proxyRuntime.Stop()
		t.Fatalf("SDK log reached terminal output while runtime was active: %q", output.String())
	}

	proxyRuntime.Stop()
	log.Warn("still hidden after embedded runtime stops")
	if output.Len() != 0 {
		t.Fatalf("late SDK log reached terminal output after runtime stopped: %q", output.String())
	}
}

func TestStartCodexAPIAdaptsResponsesRequest(t *testing.T) {
	t.Setenv(codexClientVersionEnv, "9.8.7")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM_PROGRAM_VERSION", "")
	t.Setenv("TERM", "")
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
	proxyRuntime, err := StartCodexAPI(ctx, upstream.URL+"/v1/responses", "upstream-key", "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("StartCodexAPI() error: %v", err)
	}
	runtimeDir := proxyRuntime.runtimeDir
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
	if !strings.HasPrefix(got.header.Get("User-Agent"), "codex_cli_rs/9.8.7 ") ||
		strings.Contains(got.header.Get("User-Agent"), "(codex_cli_rs;") ||
		!strings.HasSuffix(got.header.Get("User-Agent"), " unknown") {
		t.Fatalf("upstream User-Agent = %q", got.header.Get("User-Agent"))
	}
	if got.header.Get("Originator") != embeddedCodexOriginator {
		t.Fatalf("upstream Originator = %q", got.header.Get("Originator"))
	}
	if got.header.Get("Version") != "" {
		t.Fatalf("custom Codex provider must not receive Version header: %q", got.header.Get("Version"))
	}
	if got.header.Get("X-Codex-Beta-Features") != "remote_compaction_v2" {
		t.Fatalf("upstream X-Codex-Beta-Features = %q", got.header.Get("X-Codex-Beta-Features"))
	}
	if got.header.Get("Session-Id") == "" || got.header.Get("Thread-Id") == "" {
		t.Fatalf("upstream session headers are incomplete: %+v", got.header)
	}
	for key := range got.header {
		if strings.EqualFold(key, "Session_id") {
			t.Fatalf("upstream retained legacy duplicate session header %q: %+v", key, got.header)
		}
	}
	if got.header.Get("X-Client-Request-Id") != got.header.Get("Session-Id") {
		t.Fatalf("upstream X-Client-Request-Id = %q, session = %q", got.header.Get("X-Client-Request-Id"), got.header.Get("Session-Id"))
	}
	if got.header.Get("X-Codex-Window-Id") != got.header.Get("Session-Id")+":0" {
		t.Fatalf("upstream X-Codex-Window-Id = %q", got.header.Get("X-Codex-Window-Id"))
	}
	var turnMetadata map[string]any
	if err := json.Unmarshal([]byte(got.header.Get("X-Codex-Turn-Metadata")), &turnMetadata); err != nil {
		t.Fatalf("decode X-Codex-Turn-Metadata: %v", err)
	}
	if turnMetadata["session_id"] != got.header.Get("Session-Id") || turnMetadata["window_id"] != got.header.Get("X-Codex-Window-Id") {
		t.Fatalf("turn metadata does not match request headers: %+v", turnMetadata)
	}
	if _, ok := got.body["max_output_tokens"]; ok {
		t.Fatalf("Codex request retained max_output_tokens: %+v", got.body)
	}
	if stream, _ := got.body["stream"].(bool); !stream {
		t.Fatalf("Codex request did not force streaming: %+v", got.body)
	}
	clientMetadata, ok := got.body["client_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("Codex request is missing client_metadata: %+v", got.body)
	}
	if clientMetadata["session_id"] != got.header.Get("Session-Id") ||
		clientMetadata["thread_id"] != got.header.Get("Thread-Id") ||
		clientMetadata["x-codex-window-id"] != got.header.Get("X-Codex-Window-Id") {
		t.Fatalf("client_metadata does not match request headers: %+v", clientMetadata)
	}
	if clientMetadata["x-codex-turn-metadata"] != got.header.Get("X-Codex-Turn-Metadata") {
		t.Fatalf("client_metadata turn metadata does not match header: %+v", clientMetadata)
	}
	if got.body["prompt_cache_key"] != got.header.Get("Session-Id") {
		t.Fatalf("prompt_cache_key = %v, session = %q", got.body["prompt_cache_key"], got.header.Get("Session-Id"))
	}

	proxyRuntime.Stop()
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("Codex runtime directory still exists after Stop(): %v", err)
	}
}

func TestNormalizeCodexRequestIdentityUsesTranslatedPromptCacheKey(t *testing.T) {
	identity := codexRequestIdentity{
		installationID: "installation-test",
		sessionID:      "fallback-session",
		turnID:         "turn-test",
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/responses",
		strings.NewReader(`{"prompt_cache_key":"translated-session","client_metadata":{"existing":"yes"}}`),
	)
	request.Header["Session_id"] = []string{"translated-session"}
	request.Header["Session-Id"] = []string{"conflicting-session"}

	if err := normalizeCodexRequestIdentity(request, identity); err != nil {
		t.Fatalf("normalizeCodexRequestIdentity() error: %v", err)
	}
	if got := request.Header.Get("Session-Id"); got != "translated-session" {
		t.Fatalf("Session-Id = %q", got)
	}
	sessionHeaders := 0
	for key := range request.Header {
		if strings.ReplaceAll(strings.ToLower(key), "_", "-") == "session-id" {
			sessionHeaders++
		}
	}
	if sessionHeaders != 1 {
		t.Fatalf("session header variants = %d: %+v", sessionHeaders, request.Header)
	}

	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	metadata, ok := body["client_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("client_metadata missing: %+v", body)
	}
	if metadata["existing"] != "yes" || metadata["session_id"] != "translated-session" {
		t.Fatalf("client_metadata = %+v", metadata)
	}
	if body["prompt_cache_key"] != "translated-session" {
		t.Fatalf("prompt_cache_key = %v", body["prompt_cache_key"])
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

func TestStartCodexAPIServesClaudeMessages(t *testing.T) {
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
	proxyRuntime, err := StartCodexAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test,gpt-test[1m]")
	if err != nil {
		t.Fatalf("StartCodexAPI() error: %v", err)
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

func TestStartCodexAPIClaudeMessagesCompletedOutputOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_compact\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"compact summary\"}]}],\"usage\":{\"input_tokens\":40000,\"output_tokens\":20,\"total_tokens\":40020}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartCodexAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test")
	if err != nil {
		t.Fatalf("StartCodexAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test")
	if !strings.Contains(responseBody, "compact summary") || !strings.Contains(responseBody, `"type":"message_stop"`) {
		t.Fatalf("CLIProxyAPI lost completed-only compact output: %s", responseBody)
	}
	if count := strings.Count(responseBody, "compact summary"); count != 1 {
		t.Fatalf("completed-only compact output appeared %d times, want once: %s", count, responseBody)
	}
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

func TestStopUnregistersRuntimeModels(t *testing.T) {
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
	proxyRuntime, err := Start(ctx, ProviderCodex)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	auths := proxyRuntime.coreManager.List()
	if len(auths) != 1 {
		proxyRuntime.Stop()
		t.Fatalf("runtime auth count = %d, want 1", len(auths))
	}
	models := cliproxy.GlobalModelRegistry().GetAvailableModelsByProvider(ProviderCodex)
	if len(models) == 0 {
		proxyRuntime.Stop()
		t.Fatal("Codex runtime registered no models")
	}

	registeredModel := ""
	for _, model := range models {
		if model != nil && cliproxy.GlobalModelRegistry().ClientSupportsModel(auths[0].ID, model.ID) {
			registeredModel = model.ID
			break
		}
	}
	if registeredModel == "" {
		proxyRuntime.Stop()
		t.Fatal("runtime auth does not support any registered Codex model")
	}

	proxyRuntime.Stop()
	if cliproxy.GlobalModelRegistry().ClientSupportsModel(auths[0].ID, registeredModel) {
		t.Fatalf("model %q is still registered for auth %q after Stop()", registeredModel, auths[0].ID)
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
	proxyRuntime, err := Start(ctx, ProviderCodex)
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
	_, err := Start(context.Background(), ProviderGemini)
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

func TestStartOpenAIResponsesAPIDoesNotInjectCodexIdentity(t *testing.T) {
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
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"in_progress\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"plain ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"plain ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test", 0)
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test")
	if !strings.Contains(responseBody, "plain ok") {
		t.Fatalf("plain Responses runtime did not return Claude SSE: %s", responseBody)
	}

	got := <-captured
	if got.header.Get("Originator") != "" {
		t.Fatalf("plain Responses must not send Originator, got %q", got.header.Get("Originator"))
	}
	if got.header.Get("X-Codex-Beta-Features") != "" {
		t.Fatalf("plain Responses must not send X-Codex-Beta-Features, got %q", got.header.Get("X-Codex-Beta-Features"))
	}
	if _, ok := got.body["client_metadata"]; ok {
		t.Fatalf("plain Responses must not inject client_metadata: %+v", got.body)
	}
	ua := got.header.Get("User-Agent")
	if strings.Contains(strings.ToLower(ua), "codex") {
		t.Fatalf("plain Responses must not use Codex User-Agent, got %q", ua)
	}
	if ua != plainResponsesUserAgent {
		t.Fatalf("plain Responses User-Agent = %q, want %q", ua, plainResponsesUserAgent)
	}
	for key := range got.header {
		normalized := strings.ReplaceAll(strings.ToLower(key), "_", "-")
		switch normalized {
		case "session-id", "thread-id", "x-codex-window-id", "originator", "x-codex-beta-features":
			t.Fatalf("plain Responses retained Codex header %q: %+v", key, got.header)
		}
	}
}

func TestStartProviderRoutesPlainResponsesAwayFromCodexIdentity(t *testing.T) {
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
	if got.header.Get("Originator") != "" || got.body["client_metadata"] != nil {
		t.Fatalf("StartProvider plain responses still used Codex identity: headers=%v body=%v", got.header, got.body)
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
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test", 0)
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

func TestStartCodexAPIClaudeMessagesMissingCreatedBeforeDelta(t *testing.T) {
	// Upstream emits a content delta before any response.created; the compat
	// proxy must synthesize created first so CLIProxyAPI gets a sane order.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"order ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_order\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"order ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxyRuntime, err := StartOpenAIResponsesAPI(ctx, upstream.URL+"/v1", "upstream-key", "gpt-test", 0)
	if err != nil {
		t.Fatalf("StartOpenAIResponsesAPI() error: %v", err)
	}
	defer proxyRuntime.Stop()

	responseBody := postClaudeMessage(t, ctx, proxyRuntime, "gpt-test")
	if !strings.Contains(responseBody, "order ok") {
		t.Fatalf("missing content when created was delayed: %s", responseBody)
	}
	// Claude stream should still produce a normal message lifecycle.
	if !strings.Contains(responseBody, `"type":"message_start"`) || !strings.Contains(responseBody, `"type":"message_stop"`) {
		t.Fatalf("Claude lifecycle incomplete without upstream created: %s", responseBody)
	}
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

func TestStartProviderRejectsCodexV1Endpoint(t *testing.T) {
	_, err := StartProvider(context.Background(), StartOptions{
		Protocol:  ProtocolOpenAIResponses,
		Endpoint:  "https://new.sharedchat.cc/codex/v1",
		APIKey:    "test-key",
		ModelSpec: "gpt-5.4-mini",
	})
	if err == nil {
		t.Fatal("StartProvider() should reject /codex/v1 endpoints")
	}
	if !strings.Contains(err.Error(), "https://new.sharedchat.cc/codex") {
		t.Fatalf("error = %v, want suggestion for /codex without /v1", err)
	}
}
