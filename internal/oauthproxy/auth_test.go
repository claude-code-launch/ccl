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
		"gemini":  "antigravity",
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

func TestCodexBaseURLDoesNotRewriteUserBasePath(t *testing.T) {
	tests := map[string]string{
		"https://new.sharedchat.cc/codex":              "https://new.sharedchat.cc/codex",
		"https://new.sharedchat.cc/codex/v1":           "https://new.sharedchat.cc/codex/v1",
		"https://new.sharedchat.cc/codex/v1/responses": "https://new.sharedchat.cc/codex/v1",
		"https://api.openai.com/v1":                    "https://api.openai.com/v1",
		"https://example.com/api/v1/responses":         "https://example.com/api/v1",
	}
	for input, want := range tests {
		if got := codexBaseURL(input); got != want {
			t.Errorf("codexBaseURL(%q) = %q; want %q", input, got, want)
		}
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

	store := newProviderTokenStore(authDir, ProviderCodex)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 || auths[0].Provider != ProviderCodex {
		t.Fatalf("filtered auths = %+v, want one Codex auth", auths)
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
	if !strings.HasPrefix(got.header.Get("User-Agent"), "codex_exec/9.8.7 ") ||
		!strings.HasSuffix(got.header.Get("User-Agent"), "(codex_exec; 9.8.7)") {
		t.Fatalf("upstream User-Agent = %q", got.header.Get("User-Agent"))
	}
	if got.header.Get("Originator") != embeddedCodexOriginator {
		t.Fatalf("upstream Originator = %q", got.header.Get("Originator"))
	}
	if got.header.Get("X-Codex-Beta-Features") != "remote_compaction_v2" {
		t.Fatalf("upstream X-Codex-Beta-Features = %q", got.header.Get("X-Codex-Beta-Features"))
	}
	if got.header.Get("Session-Id") == "" || got.header.Get("Thread-Id") == "" {
		t.Fatalf("upstream session headers are incomplete: %+v", got.header)
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
	if got.body["prompt_cache_key"] != got.header.Get("Session-Id") {
		t.Fatalf("prompt_cache_key = %v, session = %q", got.body["prompt_cache_key"], got.header.Get("Session-Id"))
	}

	proxyRuntime.Stop()
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("Codex runtime directory still exists after Stop(): %v", err)
	}
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
