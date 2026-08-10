package oauthproxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestKiroEffectiveMachineIDShapes(t *testing.T) {
	long := strings.Repeat("ab", 32)
	if got := (&kiroCredential{machineID: strings.ToUpper(long)}).effectiveMachineID(); got != long {
		t.Fatalf("64 hex machine id = %q, want %q", got, long)
	}

	// A UUID collapses to 32 hex characters and is doubled into the 64 character
	// fingerprint the Kiro IDE sends.
	uuidLike := "8f14e45f-ceea-467a-9d61-1b0f0f0f0f0f"
	stripped := strings.ReplaceAll(uuidLike, "-", "")
	if got := (&kiroCredential{machineID: uuidLike}).effectiveMachineID(); got != stripped+stripped {
		t.Fatalf("uuid machine id = %q, want %q", got, stripped+stripped)
	}

	// Anything else falls back to a stable hash of the credential.
	credential := &kiroCredential{machineID: "not-hex", refreshToken: "refresh"}
	first := credential.effectiveMachineID()
	if len(first) != 64 {
		t.Fatalf("fallback machine id = %q, want 64 hex characters", first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("fallback machine id is not hex: %v", err)
	}
	if second := credential.effectiveMachineID(); second != first {
		t.Fatalf("fallback machine id is unstable: %q then %q", first, second)
	}
}

func writeKiroTestCredential(t *testing.T, path string, overrides map[string]any) {
	t.Helper()
	credential := map[string]any{
		"type":         "kiro",
		"access_token": "access-token",
		"profile_arn":  kiroBuilderProfileARN,
		"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"auth_method":  "social",
		"provider":     "google",
	}
	for key, value := range overrides {
		credential[key] = value
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKiroCredentialCacheReloadsChangedFiles(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "kiro-a.json")
	writeKiroTestCredential(t, path, map[string]any{"access_token": "first"})

	pool := newKiroCredentialPool(authDir, "")
	credentials, err := pool.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || credentials[0].accessToken != "first" {
		t.Fatalf("credentials = %#v", credentials)
	}

	// Callers get private copies: mutating one must not poison the cache.
	credentials[0].metadata["access_token"] = "mutated"
	again, err := pool.load()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].accessToken != "first" || metadataString(again[0].metadata, "access_token") != "first" {
		t.Fatalf("cache handed back mutated credential: %#v", again[0].metadata)
	}

	writeKiroTestCredential(t, path, map[string]any{"access_token": "second"})
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	updated, err := pool.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || updated[0].accessToken != "second" {
		t.Fatalf("credential was not reloaded after change: %#v", updated)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	removed, err := pool.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("credentials after removal = %#v", removed)
	}
	pool.cache.mu.Lock()
	remaining := len(pool.cache.entries)
	pool.cache.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cache still holds %d entries for deleted credentials", remaining)
	}
}

func TestKiroCallUpstreamKeepsUnauthorizedDiagnosticsWhenRefreshFails(t *testing.T) {
	authDir := t.TempDir()
	// No refresh_token, so the forced refresh after the 401 must fail.
	writeKiroTestCredential(t, filepath.Join(authDir, "kiro-a.json"), map[string]any{"access_token": "expired"})

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"message":"token expired upstream"}`)
	}))
	defer upstream.Close()

	service := &kiroService{
		models:      []string{"claude-sonnet-4.6"},
		pool:        newKiroCredentialPool(authDir, ""),
		client:      upstream.Client(),
		upstreamURL: func(*kiroCredential) string { return upstream.URL },
	}

	_, err := service.callUpstream(context.Background(), &kiroConvertedRequest{
		body:  map[string]any{"conversationState": map[string]any{}},
		model: "claude-sonnet-4.6",
	})
	if err == nil {
		t.Fatal("expected an error when every credential fails")
	}
	if !strings.Contains(err.Error(), "token expired upstream") {
		t.Fatalf("upstream diagnostics were lost: %v", err)
	}
	var upstreamErr *kiroUpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.status != http.StatusUnauthorized {
		t.Fatalf("error does not carry the upstream status: %v", err)
	}
}

// newKiroUpstreamTestService wires a service against handler as its upstream, with
// a rate-limit backoff short enough for a unit test.
func newKiroUpstreamTestService(t *testing.T, handler http.HandlerFunc) (*kiroService, func()) {
	t.Helper()
	authDir := t.TempDir()
	writeKiroTestCredential(t, filepath.Join(authDir, "kiro-a.json"), nil)

	upstream := httptest.NewServer(handler)
	service := &kiroService{
		apiKey:           "ccl-test-key",
		models:           kiroRuntimeModels("claude-sonnet-4.6"),
		pool:             newKiroCredentialPool(authDir, ""),
		client:           upstream.Client(),
		upstreamURL:      func(*kiroCredential) string { return upstream.URL },
		rateLimitBackoff: []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond},
	}
	return service, upstream.Close
}

func kiroRateLimitBody() string {
	return `{"message":"Too many requests, please wait before trying again.","reason":"USER_REQUEST_RATE_EXCEEDED"}`
}

func TestKiroCallUpstreamRetriesRateLimitedRequests(t *testing.T) {
	var attempts atomic.Int32
	service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(writer, kiroRateLimitBody())
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	defer closeUpstream()

	response, err := service.callUpstream(context.Background(), &kiroConvertedRequest{
		body:  map[string]any{"conversationState": map[string]any{}},
		model: "claude-sonnet-4.6",
	})
	if err != nil {
		t.Fatalf("callUpstream after transient rate limits: %v", err)
	}
	_ = response.Body.Close()
	if got := attempts.Load(); got != 3 {
		t.Fatalf("upstream attempts = %d, want 3 (initial + 2 retries)", got)
	}
}

func TestKiroCallUpstreamStopsAfterRateLimitBudget(t *testing.T) {
	var attempts atomic.Int32
	service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, kiroRateLimitBody())
	})
	defer closeUpstream()

	_, err := service.callUpstream(context.Background(), &kiroConvertedRequest{
		body:  map[string]any{"conversationState": map[string]any{}},
		model: "claude-sonnet-4.6",
	})
	if err == nil {
		t.Fatal("expected the rate limit to be returned once the budget is spent")
	}
	// One initial attempt plus one per configured backoff step.
	if got, want := attempts.Load(), int32(1+len(service.rateLimitBackoff)); got != want {
		t.Fatalf("upstream attempts = %d, want %d", got, want)
	}
	var upstreamErr *kiroUpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.status != http.StatusTooManyRequests {
		t.Fatalf("error does not carry HTTP 429: %v", err)
	}
	if !strings.Contains(err.Error(), "USER_REQUEST_RATE_EXCEEDED") {
		t.Fatalf("upstream reason was lost: %v", err)
	}
}

func TestKiroCallUpstreamRateLimitRespectsCancellation(t *testing.T) {
	var attempts atomic.Int32
	service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, kiroRateLimitBody())
	})
	defer closeUpstream()
	service.rateLimitBackoff = []time.Duration{time.Minute, time.Minute, time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for attempts.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := service.callUpstream(ctx, &kiroConvertedRequest{
		body:  map[string]any{"conversationState": map[string]any{}},
		model: "claude-sonnet-4.6",
	})
	cancel()
	if err == nil {
		t.Fatal("expected an error when the client cancels during backoff")
	}
	if attempts.Load() > 1 {
		t.Fatalf("kept retrying after cancellation: attempts = %d", attempts.Load())
	}
}

// postKiroMessages sends a minimal Anthropic Messages request through the
// service handler and returns the status plus decoded error payload.
func postKiroMessages(t *testing.T, service *kiroService) (int, string, string) {
	t.Helper()
	front := httptest.NewServer(service.handler())
	defer front.Close()

	body := `{"model":"claude-sonnet-4.6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", service.apiKey)

	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, raw)
	}
	return response.StatusCode, decoded.Error.Type, decoded.Error.Message
}

// The 429 must reach the client as a rate limit, not as HTTP 400.
func TestKiroMessagesReportsRateLimitStatus(t *testing.T) {
	service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, kiroRateLimitBody())
	})
	defer closeUpstream()

	status, errorType, message := postKiroMessages(t, service)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if errorType != "rate_limit_error" {
		t.Fatalf("error type = %q, want rate_limit_error", errorType)
	}
	if !strings.Contains(message, "USER_REQUEST_RATE_EXCEEDED") {
		t.Fatalf("error message lost the upstream reason: %s", message)
	}
}

// Upstream client errors are forwarded verbatim so Claude Code can decide how to
// react; only proxy-side and upstream server failures collapse into 502.
func TestKiroMessagesForwardsUpstreamClientErrors(t *testing.T) {
	cases := []struct {
		upstreamStatus int
		wantStatus     int
		wantType       string
	}{
		{http.StatusBadRequest, http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, http.StatusNotFound, "not_found_error"},
		{http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, "request_too_large"},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, "invalid_request_error"},
		{http.StatusInternalServerError, http.StatusBadGateway, "api_error"},
		{http.StatusServiceUnavailable, http.StatusBadGateway, "api_error"},
	}
	for _, testCase := range cases {
		t.Run(strconv.Itoa(testCase.upstreamStatus), func(t *testing.T) {
			service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.upstreamStatus)
				_, _ = io.WriteString(writer, `{"message":"upstream said no","reason":"TEST_REASON"}`)
			})
			defer closeUpstream()

			status, errorType, message := postKiroMessages(t, service)
			if status != testCase.wantStatus {
				t.Errorf("status = %d, want %d", status, testCase.wantStatus)
			}
			if errorType != testCase.wantType {
				t.Errorf("error type = %q, want %q", errorType, testCase.wantType)
			}
			if !strings.Contains(message, "TEST_REASON") {
				t.Errorf("error message lost the upstream body: %s", message)
			}
		})
	}
}

func TestKiroMessagesUnauthorizedRefreshFailureKeepsStatus(t *testing.T) {
	// The credential has no refresh token, so the forced refresh after the 401
	// fails and the wrapped upstream status must still drive the response.
	service, closeUpstream := newKiroUpstreamTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"message":"expired","reason":"TEST_REASON"}`)
	})
	defer closeUpstream()

	status, errorType, message := postKiroMessages(t, service)
	if status != http.StatusUnauthorized || errorType != "authentication_error" {
		t.Fatalf("status = %d type = %q, want 401/authentication_error", status, errorType)
	}
	if !strings.Contains(message, "credential refresh failed") {
		t.Fatalf("refresh failure was not reported: %s", message)
	}
}
