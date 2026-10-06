package oauthproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zedFreePort reserves a loopback port and releases it, so a login can be told
// to bind exactly that number.
func zedFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// zedDeliverCallback plays the browser's redirect to the loopback listener.
func zedDeliverCallback(t *testing.T, port, query string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://127.0.0.1:" + port + "/?" + query)
	if err != nil {
		t.Errorf("deliver callback: %v", err)
		return
	}
	response.Body.Close()
}

func TestDecryptZedAccessTokenRejectsUndecodableAndNonTextPayloads(t *testing.T) {
	key, publicKey, err := newZedKeypair()
	if err != nil {
		t.Fatal(err)
	}
	// Invalid under both the padded and the raw base64url alphabet.
	if _, err := decryptZedAccessToken(key, "!!! not base64 !!!"); err == nil ||
		!strings.Contains(err.Error(), "decode Zed access token") {
		t.Fatalf("undecodable ciphertext error = %v", err)
	}

	der, err := base64.URLEncoding.DecodeString(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKCS1PublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	// A payload that decrypts but is not text (or is empty) must never be stored
	// as a credential token.
	for name, payload := range map[string][]byte{"binary": {0xff, 0xfe, 0xfd}, "empty": {}} {
		encrypted, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, parsed, payload, nil)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.URLEncoding.EncodeToString(encrypted)
		if _, err := decryptZedAccessToken(key, encoded); err == nil ||
			!strings.Contains(err.Error(), "not a valid token") {
			t.Fatalf("%s payload error = %v", name, err)
		}
	}
}

func TestLoginZedRefusesASignInWithNoUsableUserId(t *testing.T) {
	installZedLoginHarness(t, "not-a-number", "account-token", func(http.ResponseWriter, *http.Request) {})
	if _, err := loginZed(t.Context(), t.TempDir(), LoginOptions{}); err == nil ||
		!strings.Contains(err.Error(), "invalid user id") {
		t.Fatalf("loginZed() error = %v", err)
	}
}

func TestLoginZedRefusesAnUndecryptableCallback(t *testing.T) {
	harness := installZedLoginHarness(t, "7", "account-token", func(http.ResponseWriter, *http.Request) {})
	harness.opener = func(port, query string) {
		zedDeliverCallback(t, port, url.Values{"user_id": {"7"}, "access_token": {"not-a-ciphertext"}}.Encode())
	}
	if _, err := loginZed(t.Context(), t.TempDir(), LoginOptions{}); err == nil ||
		!strings.Contains(err.Error(), "decrypt Zed access token") {
		t.Fatalf("loginZed() error = %v", err)
	}
}

func TestLoginZedBindsTheRequestedCallbackPort(t *testing.T) {
	harness := installZedLoginHarness(t, "7", "account-token", func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{"user": map[string]any{"github_login": "octo"}})
	})
	port := zedFreePort(t)
	harness.wantPort = port
	if _, err := loginZed(t.Context(), t.TempDir(), LoginOptions{CallbackPort: port}); err != nil {
		t.Fatalf("loginZed() with a fixed callback port: %v", err)
	}
}

func TestLoginZedReportsAPortItCannotBind(t *testing.T) {
	installZedLoginHarness(t, "7", "account-token", func(http.ResponseWriter, *http.Request) {})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if _, err := loginZed(t.Context(), t.TempDir(), LoginOptions{CallbackPort: port}); err == nil ||
		!strings.Contains(err.Error(), "listen for Zed sign-in callback") {
		t.Fatalf("loginZed() on a taken port = %v", err)
	}
}

// TestLoginZedWithoutABrowserNeverOpensOne covers --no-browser: the URL is
// printed and the browser is left alone. Nothing delivers the redirect, so the
// sign-in stays open until the caller gives up — which is exactly the state a
// headless user is in when they paste the URL elsewhere.
func TestLoginZedWithoutABrowserNeverOpensOne(t *testing.T) {
	installZedLoginHarness(t, "7", "account-token", func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{"user": map[string]any{"name": "Octo Cat"}})
	})
	opened := false
	zedBrowserOpener = func(string) error {
		opened = true
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := loginZed(ctx, t.TempDir(), LoginOptions{NoBrowser: true}); err == nil {
		t.Fatal("an abandoned headless sign-in reported success")
	}
	if opened {
		t.Fatal("--no-browser opened a browser")
	}
}

func TestLoginZedAcceptsANilContextAndKeepsTheFreePlanNote(t *testing.T) {
	installZedLoginHarness(t, "7", "account-token", func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{
			"user": map[string]any{"github_login": "octo"},
			"plan": map[string]any{"plan_v3": "zed_free"},
		})
	})
	// A nil context arrives from callers that have no request to thread; the
	// login must treat it as background rather than panic.
	var noContext context.Context
	result, err := loginZed(noContext, t.TempDir(), LoginOptions{})
	if err != nil {
		t.Fatalf("loginZed(nil, ...) error = %v", err)
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["plan"] != "zed_free" {
		t.Fatalf("stored plan = %v", stored["plan"])
	}
}

func TestLoginZedReportsAnUnwritableCredentialDirectory(t *testing.T) {
	installZedLoginHarness(t, "7", "account-token", func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{"user": map[string]any{"github_login": "octo"}})
	})
	// The auth directory does not exist, so the atomic write cannot start.
	if _, err := loginZed(t.Context(), filepath.Join(t.TempDir(), "missing"), LoginOptions{}); err == nil ||
		!strings.Contains(err.Error(), "temporary credential") {
		t.Fatalf("loginZed() into a missing directory = %v", err)
	}
}

func TestLoginZedEndsWhenTheSignInIsAbandoned(t *testing.T) {
	installZedLoginHarness(t, "7", "account-token", func(http.ResponseWriter, *http.Request) {})
	// Nothing delivers the redirect, so only the cancelled context can end the
	// wait — and it must, instead of blocking until the 5-minute timeout.
	zedBrowserOpener = func(string) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loginZed(ctx, t.TempDir(), LoginOptions{}); err == nil || !strings.Contains(err.Error(), "sign-in") {
		t.Fatalf("cancelled sign-in error = %v", err)
	}
}

func TestZedCallbackDiscardsRepeatedRedirects(t *testing.T) {
	results := make(chan zedLoginCallback, 1)
	handler := zedCallbackHandler(results, "https://zed.example/done")
	for range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?user_id=7&access_token=abc", nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("status = %d", recorder.Code)
		}
	}
	if len(results) != 1 {
		t.Fatalf("queued %d callbacks, want 1", len(results))
	}
}
