package oauthproxy

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1" // #nosec G505 -- Chromium safeStorage compatibility test.
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

func TestDecryptAutoClawStoredValue(t *testing.T) {
	password := "chromium-safe-storage-test"
	plain := "access-token-from-electron"
	key := pbkdf2.Key([]byte(password), []byte(autoclawSafeStorageSalt), autoclawSafeStorageRounds, 16, sha1.New) // #nosec G505
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte(plain), bytes.Repeat([]byte{byte(aes.BlockSize - len(plain)%aes.BlockSize)}, aes.BlockSize-len(plain)%aes.BlockSize)...)
	ciphertext := make([]byte, len(padded))
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	stored := autoclawEncryptedPrefix + base64.StdEncoding.EncodeToString(append([]byte(autoclawChromiumPrefix), ciphertext...))

	got, err := decryptAutoClawStoredValue(stored, password)
	if err != nil {
		t.Fatalf("decryptAutoClawStoredValue() error: %v", err)
	}
	if got != plain {
		t.Fatalf("decrypted token = %q, want %q", got, plain)
	}
}

func TestAutoClawRefreshPersistsRotatedTokens(t *testing.T) {
	authDir := t.TempDir()
	credentialPath := filepath.Join(authDir, "autoclaw-refresh.json")
	initial := map[string]any{
		"type":          ProviderAutoClaw,
		"access_token":  "old-access-token",
		"refresh_token": "old-refresh-token",
		"device_id":     "device-refresh-test",
		"app_version":   "1.18.5",
	}
	writeAutoClawTestJSON(t, credentialPath, initial)

	type requestInfo struct {
		path          string
		appID         string
		timestamp     string
		signature     string
		authorization string
		body          map[string]any
	}
	requests := make(chan requestInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make(map[string]any)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- requestInfo{
			path: r.URL.Path, appID: r.Header.Get("X-Auth-Appid"),
			timestamp: r.Header.Get("X-Auth-TimeStamp"), signature: r.Header.Get("X-Auth-Sign"),
			authorization: r.Header.Get("Authorization"), body: body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"new-access-token","refresh_token":"new-refresh-token"}}`)
	}))
	t.Cleanup(server.Close)
	originalOrigin, originalRefresh, originalAgent := autoclawAPIOrigin, autoclawRefreshURL, autoclawAgentRefreshURL
	autoclawAPIOrigin = server.URL
	autoclawRefreshURL = server.URL + "/userapi/v1/refresh"
	autoclawAgentRefreshURL = ""
	t.Cleanup(func() {
		autoclawAPIOrigin, autoclawRefreshURL, autoclawAgentRefreshURL = originalOrigin, originalRefresh, originalAgent
	})

	authorizer := &autoClawOAuthAuthorizer{path: credentialPath, client: server.Client()}
	got, err := authorizer.authorize(context.Background(), true)
	if err != nil {
		t.Fatalf("authorize(force refresh) error: %v", err)
	}
	if got != "new-access-token" {
		t.Fatalf("access token = %q", got)
	}
	request := <-requests
	if request.path != "/userapi/v1/refresh" || request.appID != autoclawRefreshAppID || request.authorization != "Bearer old-access-token" {
		t.Fatalf("refresh request = %+v", request)
	}
	if request.body["source_id"] != "autoclaw" || request.body["device_id"] != "device-refresh-test" || request.body["refresh_token"] != "old-refresh-token" {
		t.Fatalf("refresh body = %+v", request.body)
	}
	if request.timestamp == "" || request.signature == "" {
		t.Fatalf("refresh signing headers are incomplete: %+v", request)
	}

	persisted := readAutoClawTestJSON(t, credentialPath)
	if persisted["access_token"] != "new-access-token" || persisted["refresh_token"] != "new-refresh-token" {
		t.Fatalf("persisted rotated tokens = %+v", persisted)
	}
}

func TestAutoClawRefreshCoalescesAcrossAuthorizers(t *testing.T) {
	authDir := t.TempDir()
	credentialPath := filepath.Join(authDir, "autoclaw-shared.json")
	writeAutoClawTestJSON(t, credentialPath, map[string]any{
		"type":          ProviderAutoClaw,
		"access_token":  "shared-old-access",
		"refresh_token": "shared-old-refresh",
		"device_id":     "shared-device",
		"app_version":   "1.18.5",
	})

	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(75 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"code":0,"data":{"access_token":"shared-new-access","refresh_token":"shared-new-refresh"}}`)
	}))
	t.Cleanup(server.Close)
	originalRefresh, originalAgent := autoclawRefreshURL, autoclawAgentRefreshURL
	autoclawRefreshURL = server.URL
	autoclawAgentRefreshURL = ""
	t.Cleanup(func() { autoclawRefreshURL, autoclawAgentRefreshURL = originalRefresh, originalAgent })

	authorizers := []*autoClawOAuthAuthorizer{
		{path: credentialPath, client: server.Client()},
		{path: credentialPath, client: server.Client()},
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, len(authorizers))
	for _, authorizer := range authorizers {
		wg.Add(1)
		go func(a *autoClawOAuthAuthorizer) {
			defer wg.Done()
			token, err := a.authorize(context.Background(), true)
			if err == nil && token != "shared-new-access" {
				err = fmt.Errorf("access token = %q", token)
			}
			errorsCh <- err
		}(authorizer)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want one coalesced refresh", refreshCalls.Load())
	}
	if _, err := os.Stat(credentialPath + ".refresh.lock"); !os.IsNotExist(err) {
		t.Fatalf("refresh lock was not released: %v", err)
	}
}

func TestAutoClawManagedChatRuntimeUsesDesktopContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialFile := "autoclaw-runtime.json"
	writeAutoClawTestJSON(t, filepath.Join(authDir, credentialFile), map[string]any{
		"type":          ProviderAutoClaw,
		"access_token":  "runtime-access-token",
		"refresh_token": "runtime-refresh-token",
		"device_id":     "device-runtime-test",
		"app_version":   "1.18.5",
	})

	type requestInfo struct {
		path           string
		accept         string
		userAgent      string
		authorization  string
		xAuthorization string
		xRequestModel  string
		xSessionID     string
		xAgentID       string
		xInvocationID  string
		xHarness       string
		xClientType    string
		xProduct       string
		xVersion       string
		body           map[string]any
	}
	requests := make(chan requestInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make(map[string]any)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- requestInfo{
			path: r.URL.Path, accept: r.Header.Get("Accept"), userAgent: r.Header.Get("User-Agent"),
			authorization:  r.Header.Get("Authorization"),
			xAuthorization: r.Header.Get("X-Authorization"), xRequestModel: r.Header.Get("X-Request-Model"),
			xSessionID: r.Header.Get("X-Session-Id"), xAgentID: r.Header.Get("X-Agent-Id"),
			xInvocationID: r.Header.Get("X-ZCode-Invocation-Id"),
			xHarness:      r.Header.Get("X-Harness-Type"), xClientType: r.Header.Get("X-Client-Type"),
			xProduct: r.Header.Get("X-Product"), xVersion: r.Header.Get("X-Version"), body: body,
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"managed ok\"},\"index\":0,\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	originalOrigin := autoclawAPIOrigin
	autoclawAPIOrigin = server.URL
	t.Cleanup(func() { autoclawAPIOrigin = originalOrigin })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proxyRuntime, err := startAutoClawOAuth(ctx, "", "zai_glm-5.3-flash", credentialFile)
	if err != nil {
		t.Fatalf("startAutoClawOAuth() error: %v", err)
	}
	t.Cleanup(proxyRuntime.Stop)
	// Claude Code receives the readable catalog label; the adapter must map it
	// back to AutoClaw's provider-prefixed route ID before sending upstream.
	response := postClaudeMessage(t, ctx, proxyRuntime, "GLM-5.3-Flash")
	if !strings.Contains(response, "managed ok") || !strings.Contains(response, `"type":"message_stop"`) {
		t.Fatalf("managed Chat response = %s", response)
	}
	request := <-requests
	if request.path != "/autoclaw-proxy/proxy/autoclaw/chat/completions" {
		t.Fatalf("managed Chat path = %q", request.path)
	}
	if request.authorization != "" || request.xAuthorization != "Bearer runtime-access-token" {
		t.Fatalf("managed auth headers = Authorization %q / X-Authorization %q", request.authorization, request.xAuthorization)
	}
	// The origin is fronted by an Aliyun WAF that blocks Go's default
	// Go-http-client/1.1 agent with a 405 HTML block page; the desktop app's
	// undici broker sends UA "node".
	if request.userAgent != "node" {
		t.Fatalf("managed user agent = %q", request.userAgent)
	}
	if request.accept != "*/*" || request.xRequestModel != "zai_glm-5.3-flash" || request.xHarness != "zcode" || request.xClientType != "pc" || request.xProduct != "autoclaw" || request.xVersion != "1.18.5" {
		t.Fatalf("managed routing headers = %+v", request)
	}
	if request.xSessionID == "" || request.xAgentID != "auto-coder" || request.xInvocationID == "" {
		t.Fatalf("managed request context headers = %+v", request)
	}
	if request.body["model"] != "glm-5.3-flash" {
		t.Fatalf("managed body model = %v", request.body["model"])
	}
}

func TestAutoClawManagedChatDoesNotReplayAmbiguousServerErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialFile := "autoclaw-no-retry.json"
	writeAutoClawTestJSON(t, filepath.Join(authDir, credentialFile), map[string]any{
		"type": ProviderAutoClaw, "access_token": "runtime-access-token",
		"refresh_token": "runtime-refresh-token", "device_id": "device-runtime-test",
		"app_version": "1.18.5",
	})

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(writer, `{"message":"parse response failed"}`)
	}))
	t.Cleanup(server.Close)
	originalOrigin := autoclawAPIOrigin
	autoclawAPIOrigin = server.URL
	t.Cleanup(func() { autoclawAPIOrigin = originalOrigin })

	runtime, err := startAutoClawOAuth(context.Background(), "", "zai_glm-5.3-flash", credentialFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	payload := strings.NewReader(`{"model":"GLM-5.3-Flash","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	request, _ := http.NewRequest(http.MethodPost, runtime.Endpoint()+"/messages", payload)
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if attempts.Load() != 1 {
		t.Fatalf("ambiguous managed request attempts = %d, want 1", attempts.Load())
	}
}

func writeAutoClawTestJSON(t *testing.T, path string, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAutoClawTestJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := make(map[string]any)
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
