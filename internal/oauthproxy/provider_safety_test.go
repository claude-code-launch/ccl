package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeQoderSafetyCredential(t *testing.T, dir, name string, metadata map[string]any) {
	t.Helper()
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func qoderSafetyMetadata() map[string]any {
	return map[string]any{"type": ProviderQoder, "access_token": "fake-old", "refresh_token": "fake-refresh",
		"user_id": "user-1", "machine_id": "machine-1", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}
}

func TestProviderRefreshNeverRedirectsCredentialsToAnotherOrigin(t *testing.T) {
	for _, backend := range []string{ProviderQoder, ProviderWorkBuddy, ProviderWorkBuddyCN} {
		t.Run(backend, func(t *testing.T) {
			var leaked atomic.Int32
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				leaked.Add(1)
				_, _ = w.Write([]byte(`{"token":"fake-new","expires_in":3600,"code":0,"data":{"accessToken":"fake-new","expiresIn":3600}}`))
			}))
			defer sink.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", sink.URL+"/sink?token=fake-do-not-log")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			dir := t.TempDir()
			var err error
			if backend == ProviderQoder {
				previous := qoderCenterBaseURL
				qoderCenterBaseURL = source.URL
				t.Cleanup(func() { qoderCenterBaseURL = previous })
				writeQoderSafetyCredential(t, dir, "qoder.json", qoderSafetyMetadata())
				pool := newQoderCredentialPool(dir, "qoder.json")
				credentials, loadErr := pool.load()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				_, err = pool.usable(context.Background(), credentials[0], false)
			} else {
				restoreWorkBuddyTestBaseURL(t, backend, source.URL)
				writeWorkBuddyTestCredentialFor(t, backend, dir, "workbuddy.json", "fake-old", "fake-refresh", time.Now().Add(-time.Hour))
				store := newWorkBuddyCredentialStore(workbuddyTestDeployment(t, backend), dir, "workbuddy.json")
				_, err = store.authorize(context.Background(), false)
			}
			if err == nil || leaked.Load() != 0 || strings.Contains(err.Error(), "fake-do-not-log") {
				t.Fatalf("credential redirect was not refused safely: err=%v requests=%d", err, leaked.Load())
			}
		})
	}
}

func TestProviderCredentialClientAllowsSameOriginRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/end")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	clients := []*http.Client{
		newQoderCredentialPool(t.TempDir(), "qoder.json").client,
		newWorkBuddyCredentialStore(workbuddyDeployment{provider: ProviderWorkBuddy, baseURL: server.URL}, t.TempDir(), "workbuddy.json").client,
	}
	for _, client := range clients {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/start", nil)
		response, err := doCredentialRequest(client, request)
		if err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("same-origin redirect failed: %v", err)
		}
		_ = response.Body.Close()
		client.CloseIdleConnections()
	}
}

func TestProviderNetworkErrorDoesNotExposeLoginVerifier(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	client := newQoderCredentialPool(t.TempDir(), "qoder.json").client
	defer client.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/poll?verifier=fake-query-secret", nil)
	if _, err := doCredentialRequest(client, request); err == nil || strings.Contains(err.Error(), "fake-query-secret") {
		t.Fatalf("poll error exposed login verifier: %v", err)
	}
}

func TestQoderRefreshRechecksDisabledAndCredentialType(t *testing.T) {
	for _, change := range []string{"disabled", "type"} {
		t.Run(change, func(t *testing.T) {
			var refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				_, _ = w.Write([]byte(`{"token":"fake-new","expires_in":3600}`))
			}))
			defer server.Close()
			previous := qoderCenterBaseURL
			qoderCenterBaseURL = server.URL
			t.Cleanup(func() { qoderCenterBaseURL = previous })
			dir := t.TempDir()
			metadata := qoderSafetyMetadata()
			writeQoderSafetyCredential(t, dir, "qoder.json", metadata)
			pool := newQoderCredentialPool(dir, "qoder.json")
			credentials, err := pool.load()
			if err != nil {
				t.Fatal(err)
			}
			if change == "disabled" {
				metadata["disabled"] = true
			} else {
				metadata["type"] = ProviderWorkBuddy
			}
			writeQoderSafetyCredential(t, dir, "qoder.json", metadata)
			if _, err = pool.usable(context.Background(), credentials[0], false); err == nil || refreshes.Load() != 0 {
				t.Fatalf("changed credential was refreshed: err=%v refreshes=%d", err, refreshes.Load())
			}
		})
	}
}

func TestQoderBoundCredentialDoesNotMatchAnotherCaseSensitiveFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a case-sensitive filesystem")
	}
	dir := t.TempDir()
	writeQoderSafetyCredential(t, dir, "chosen.json", qoderSafetyMetadata())
	other := qoderSafetyMetadata()
	other["user_id"] = "other-user"
	writeQoderSafetyCredential(t, dir, "CHOSEN.json", other)
	pool := newQoderCredentialPool(dir, "chosen.json")
	credentials, err := pool.load()
	if err != nil || len(credentials) != 1 || credentials[0].userID != "user-1" {
		t.Fatalf("bound account was ambiguous: count=%d err=%v", len(credentials), err)
	}
}

func TestQoderRelaysRetryAfterWithoutExtraUpstreamRequests(t *testing.T) {
	staticFastRetryBackoff(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer server.Close()
	previous := qoderAPIBaseURL
	qoderAPIBaseURL = server.URL
	t.Cleanup(func() { qoderAPIBaseURL = previous })
	dir := t.TempDir()
	metadata := qoderSafetyMetadata()
	metadata["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeQoderSafetyCredential(t, dir, "qoder.json", metadata)
	pool := newQoderCredentialPool(dir, "qoder.json")
	service := qoderService{apiKey: "fake-loopback", pool: pool, client: pool.client, models: []qoderModel{{ID: "qmodel"}}}
	request := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBufferString(`{"model":"qmodel","max_tokens":32,"messages":[{"role":"user","content":"local fixture"}]}`))
	request.Header.Set("X-Api-Key", "fake-loopback")
	recorder := httptest.NewRecorder()
	service.handleMessages(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "60" || attempts.Load() != 1 {
		t.Fatalf("Qoder cooldown was lost: status=%d retry=%q attempts=%d", recorder.Code, recorder.Header().Get("Retry-After"), attempts.Load())
	}
}

func TestQoderFailedRefreshPreservesAuthenticationFailure(t *testing.T) {
	var chats, refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "refresh_token") {
			refreshes.Add(1)
			http.Error(w, "refresh rejected", http.StatusUnauthorized)
			return
		}
		chats.Add(1)
		http.Error(w, "subscription authentication required", http.StatusUnauthorized)
	}))
	defer server.Close()
	previousAPI, previousCenter := qoderAPIBaseURL, qoderCenterBaseURL
	qoderAPIBaseURL, qoderCenterBaseURL = server.URL, server.URL
	t.Cleanup(func() { qoderAPIBaseURL, qoderCenterBaseURL = previousAPI, previousCenter })
	dir := t.TempDir()
	metadata := qoderSafetyMetadata()
	metadata["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeQoderSafetyCredential(t, dir, "qoder.json", metadata)
	pool := newQoderCredentialPool(dir, "qoder.json")
	service := qoderService{apiKey: "fake-loopback", pool: pool, client: pool.client, models: []qoderModel{{ID: "qmodel"}}}
	request := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBufferString(`{"model":"qmodel","messages":[{"role":"user","content":"local fixture"}]}`))
	request.Header.Set("X-Api-Key", "fake-loopback")
	recorder := httptest.NewRecorder()
	service.handleMessages(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "subscription authentication required") || chats.Load() != 1 || refreshes.Load() != 1 {
		t.Fatalf("auth rejection was changed into a retryable failure: status=%d chats=%d refreshes=%d", recorder.Code, chats.Load(), refreshes.Load())
	}
}
