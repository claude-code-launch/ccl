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
	"strings"
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

	pool := newKiroCredentialPool(authDir, nil, false, nil)
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
		pool:        newKiroCredentialPool(authDir, nil, false, nil),
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
