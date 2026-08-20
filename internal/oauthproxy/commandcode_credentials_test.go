package oauthproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commandcodeTestHome redirects HOME into a temp dir and returns both the
// home dir and the ccl auth dir beneath it. t.Setenv forbids t.Parallel, and
// every login test needs its own HOME so none of them can leak into the real
// ~/.commandcode or ~/.ccl.
func commandcodeTestHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir, err := AuthDir()
	if err != nil {
		t.Fatalf("AuthDir: %v", err)
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	return home, authDir
}

// commandcodeTestWhoami serves GET /alpha/whoami with the same header contract
// the official gateway enforces. Accepts exactly one bearer key.
func commandcodeTestWhoami(validKey string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/alpha/whoami" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("x-cli-environment") != "production" || request.Header.Get("x-command-code-version") != commandcodeVersion {
			http.Error(writer, `{"error":{"message":"missing identity"}}`, http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+validKey {
			http.Error(writer, `{"error":{"message":"invalid token"}}`, http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}
}

func writeOfficialCommandCodeAuth(t *testing.T, home, apiKey string) {
	t.Helper()
	path := filepath.Join(home, ".commandcode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir official auth dir: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"apiKey":          apiKey,
		"userId":          "u-123",
		"userName":        "Ada",
		"keyName":         "Mac Studio",
		"authenticatedAt": "2026-08-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal official auth: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write official auth: %v", err)
	}
}

func TestLoginCommandCodeImportsOfficialCredential(t *testing.T) {
	home, authDir := commandcodeTestHome(t)
	upstream := httptest.NewServer(commandcodeTestWhoami("user_test_key"))
	defer upstream.Close()
	t.Setenv("COMMANDCODE_API_URL", upstream.URL)
	writeOfficialCommandCodeAuth(t, home, "user_test_key")

	result, err := loginCommandCode(context.Background(), authDir)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.Provider != ProviderCommandCode || result.Backend != ProviderCommandCode {
		t.Fatalf("result = %+v", result)
	}
	if want := filepath.Join(authDir, commandcodeCredentialFile); result.Path != want {
		t.Fatalf("credential path = %q, want %q", result.Path, want)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	for key, want := range map[string]string{
		"type":             ProviderCommandCode,
		"api_key":          "user_test_key",
		"user_id":          "u-123",
		"user_name":        "Ada",
		"key_name":         "Mac Studio",
		"authenticated_at": "2026-08-01T00:00:00Z",
		"source":           "official_cli_import",
	} {
		if saved[key] != want {
			t.Errorf("credential[%q] = %v, want %q", key, saved[key], want)
		}
	}
	apiKey, metadata, err := loadCommandCodeCredential(authDir, commandcodeCredentialFile)
	if err != nil {
		t.Fatalf("load credential: %v", err)
	}
	if apiKey != "user_test_key" || metadata["user_name"] != "Ada" {
		t.Fatalf("loaded key=%q metadata=%v", apiKey, metadata)
	}
	auths := commandcodeListAuths(result.Path)
	if len(auths) != 1 || auths[0].Label != "Ada" || auths[0].Status != StatusActive {
		t.Fatalf("listAuths = %+v", auths)
	}
}

func TestLoginCommandCodeRejectsInvalidKey(t *testing.T) {
	home, authDir := commandcodeTestHome(t)
	upstream := httptest.NewServer(commandcodeTestWhoami("user_good_key"))
	defer upstream.Close()
	t.Setenv("COMMANDCODE_API_URL", upstream.URL)
	writeOfficialCommandCodeAuth(t, home, "user_stale_key")

	_, err := loginCommandCode(context.Background(), authDir)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("login error = %v, want key-rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(authDir, commandcodeCredentialFile)); !os.IsNotExist(statErr) {
		t.Fatalf("credential should not be written on rejected key, stat err=%v", statErr)
	}
}

func TestLoginCommandCodeMissingOfficialFile(t *testing.T) {
	_, authDir := commandcodeTestHome(t)
	_, err := loginCommandCode(context.Background(), authDir)
	if err == nil || !strings.Contains(err.Error(), "official Command Code credential") {
		t.Fatalf("login error = %v, want official-file hint", err)
	}
}

func TestStartCommandCodeOAuthServesImportedCredential(t *testing.T) {
	_, authDir := commandcodeTestHome(t)
	// The upstream is never contacted at start: the handshake runs lazily before
	// the first generate request.
	t.Setenv("COMMANDCODE_API_URL", "http://127.0.0.1:1")
	credential := filepath.Join(authDir, commandcodeCredentialFile)
	payload, err := json.Marshal(map[string]any{
		"type":      ProviderCommandCode,
		"api_key":   "user_runtime_key",
		"user_name": "Ada",
	})
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if err := os.WriteFile(credential, payload, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	runtime, err := startCommandCodeOAuth(context.Background(), "", commandcodeCredentialFile)
	if err != nil {
		t.Fatalf("start oauth: %v", err)
	}
	defer runtime.Stop()
	if got := len(runtime.Models()); got != len(commandcodeModelCatalog) {
		t.Fatalf("runtime models = %d, want %d", got, len(commandcodeModelCatalog))
	}
	auths := runtime.ListAuths()
	if len(auths) != 1 || auths[0].Label != "Ada" || auths[0].Status != StatusActive {
		t.Fatalf("ListAuths = %+v", auths)
	}
}

func TestStartCommandCodeOAuthRejectsForeignCredential(t *testing.T) {
	_, authDir := commandcodeTestHome(t)
	credential := filepath.Join(authDir, commandcodeCredentialFile)
	payload, err := json.Marshal(map[string]any{"type": ProviderKimi, "api_key": "user_wrong_type"})
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if err := os.WriteFile(credential, payload, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	if _, err := startCommandCodeOAuth(context.Background(), "", commandcodeCredentialFile); err == nil {
		t.Fatal("start with a non-commandcode credential should fail")
	}
}
