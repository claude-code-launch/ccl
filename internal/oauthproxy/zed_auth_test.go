package oauthproxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// encryptForZed plays Zed's server: it encrypts a token to the public key the
// sign-in URL carries, with OAEP-SHA256 (current) or PKCS#1 v1.5 (legacy).
func encryptForZed(t *testing.T, publicKey, token string, oaep bool) string {
	t.Helper()
	der, err := base64.URLEncoding.DecodeString(publicKey)
	if err != nil {
		t.Fatalf("public key is not padded base64url: %v", err)
	}
	parsed, err := x509.ParsePKCS1PublicKey(der)
	if err != nil {
		t.Fatalf("public key is not PKCS#1 DER: %v", err)
	}
	var encrypted []byte
	if oaep {
		encrypted, err = rsa.EncryptOAEP(sha256.New(), rand.Reader, parsed, []byte(token), nil)
	} else {
		encrypted, err = rsa.EncryptPKCS1v15(rand.Reader, parsed, []byte(token))
	}
	if err != nil {
		t.Fatal(err)
	}
	return base64.URLEncoding.EncodeToString(encrypted)
}

func TestDecryptZedAccessTokenAcceptsOAEPAndLegacyPKCS1(t *testing.T) {
	key, publicKey, err := newZedKeypair()
	if err != nil {
		t.Fatal(err)
	}
	for name, oaep := range map[string]bool{"oaep-sha256": true, "pkcs1v15": false} {
		encrypted := encryptForZed(t, publicKey, "secret-token-"+name, oaep)
		got, err := decryptZedAccessToken(key, encrypted)
		if err != nil || got != "secret-token-"+name {
			t.Fatalf("%s: got %q, %v", name, got, err)
		}
		// Browsers may drop base64 padding when echoing the query value.
		if got, err := decryptZedAccessToken(key, strings.TrimRight(encrypted, "=")); err != nil || got != "secret-token-"+name {
			t.Fatalf("%s unpadded: got %q, %v", name, got, err)
		}
	}
	if _, err := decryptZedAccessToken(key, "not-valid-ciphertext"); err == nil {
		t.Fatal("garbage ciphertext decrypted")
	}
	other, _, err := newZedKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptZedAccessToken(other, encryptForZed(t, publicKey, "x", true)); err == nil {
		t.Fatal("token encrypted to another key decrypted")
	}
}

func TestZedCallbackHandlerIgnoresStrayRequestsUntilTokenArrives(t *testing.T) {
	results := make(chan zedLoginCallback, 1)
	handler := zedCallbackHandler(results, "https://zed.example/native_app_signin_succeeded")

	for _, target := range []string{"/favicon.ico", "/?user_id=7", "/?access_token=abc"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest || len(results) != 0 {
			t.Fatalf("%s: status=%d queued=%d, want rejected and nothing queued", target, recorder.Code, len(results))
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?user_id=7&access_token=abc%3D%3D", nil))
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://zed.example/native_app_signin_succeeded" {
		t.Fatalf("accepted callback = %d Location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
	got := <-results
	if got.userID != "7" || got.accessToken != "abc==" {
		t.Fatalf("callback = %+v", got)
	}
}

type zedLoginHarness struct {
	signInURL chan *url.URL
	cloud     *httptest.Server
	// opener replaces the default redirect when set: it receives the callback
	// port the login bound and, when wantPort is non-zero, the port it was asked
	// to bind. A nil opener performs the sign-in the way zed.dev does.
	opener   func(port, query string)
	wantPort int
}

// installZedLoginHarness points login at a fake Zed and replaces the browser
// with one that performs the redirect exactly as zed.dev does.
func installZedLoginHarness(t *testing.T, userID, token string, profile http.HandlerFunc) *zedLoginHarness {
	t.Helper()
	harness := &zedLoginHarness{signInURL: make(chan *url.URL, 1)}
	harness.cloud = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/client/users/me" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != userID+" "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		profile(writer, request)
	}))
	t.Cleanup(harness.cloud.Close)

	previousServer, previousCloud, previousOpener := zedServerBaseURL, zedCloudBaseURL, zedBrowserOpener
	zedServerBaseURL, zedCloudBaseURL = "https://zed.example", harness.cloud.URL
	zedBrowserOpener = func(target string) error {
		parsed, err := url.Parse(target)
		if err != nil {
			t.Errorf("sign-in URL: %v", err)
			return err
		}
		harness.signInURL <- parsed
		query := parsed.Query()
		if harness.wantPort != 0 && query.Get("native_app_port") != strconv.Itoa(harness.wantPort) {
			t.Errorf("callback port = %q, want %d", query.Get("native_app_port"), harness.wantPort)
		}
		if harness.opener != nil {
			harness.opener(query.Get("native_app_port"), url.Values{
				"user_id":      {userID},
				"access_token": {encryptForZed(t, query.Get("native_app_public_key"), token, true)},
			}.Encode())
			return nil
		}
		callback := url.Values{
			"user_id":      {userID},
			"access_token": {encryptForZed(t, query.Get("native_app_public_key"), token, true)},
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Get("http://127.0.0.1:" + query.Get("native_app_port") + "/?" + callback.Encode())
		if err != nil {
			t.Errorf("callback: %v", err)
			return err
		}
		defer response.Body.Close()
		if got := response.Header.Get("Location"); response.StatusCode != http.StatusFound || got != "https://zed.example/native_app_signin_succeeded" {
			t.Errorf("callback response = %d Location=%q", response.StatusCode, got)
		}
		return nil
	}
	t.Cleanup(func() {
		zedServerBaseURL, zedCloudBaseURL, zedBrowserOpener = previousServer, previousCloud, previousOpener
	})
	return harness
}

func TestLoginZedStoresVerifiedCredentialAfterBrowserHandshake(t *testing.T) {
	harness := installZedLoginHarness(t, "4242", "account-token", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(zedSystemIDHeader) == "" || !strings.HasPrefix(request.Header.Get("User-Agent"), "Zed/") {
			t.Errorf("profile request identity headers: system=%q ua=%q", request.Header.Get(zedSystemIDHeader), request.Header.Get("User-Agent"))
		}
		writeTestJSON(writer, map[string]any{
			"user":                    map[string]any{"github_login": "Octo-Cat", "name": "Octo Cat"},
			"organizations":           []map[string]any{{"id": "org_personal", "name": "Octo", "is_personal": true}},
			"default_organization_id": "org_personal",
			"plan":                    map[string]any{"plan_v3": "zed_pro"},
		})
	})

	authDir := t.TempDir()
	result, err := loginZed(t.Context(), authDir, LoginOptions{})
	if err != nil {
		t.Fatalf("loginZed() error: %v", err)
	}

	signIn := <-harness.signInURL
	if signIn.Scheme+"://"+signIn.Host+signIn.Path != "https://zed.example/native_app_signin" {
		t.Fatalf("sign-in URL = %s", signIn)
	}
	query := signIn.Query()
	if query.Get("native_app_port") == "" || query.Get("native_app_public_key") == "" || query.Get("system_id") == "" {
		t.Fatalf("sign-in query is missing native-app parameters: %v", query)
	}

	if result.Provider != ProviderZed || result.Backend != ProviderZed || filepath.Base(result.Path) != "zed-octo-cat.json" {
		t.Fatalf("result = %+v", result)
	}
	info, err := os.Stat(result.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"type": "zed", "user_id": "4242", "access_token": "account-token", "login": "Octo-Cat",
		"organization_id": "org_personal", "plan": "zed_pro", "system_id": query.Get("system_id"),
	} {
		if stored[key] != want {
			t.Fatalf("stored[%q] = %v, want %q", key, stored[key], want)
		}
	}

	// The stored file is exactly what the runtime loads.
	credential, err := loadZedCredential(authDir, filepath.Base(result.Path))
	if err != nil || credential.auth.authorization() != "4242 account-token" {
		t.Fatalf("loadZedCredential = %+v, %v", credential, err)
	}
}

func TestLoginZedRejectsAccountTokenZedDoesNotAccept(t *testing.T) {
	// The cloud authorizes only "4242 other-token", so the profile fetch 401s.
	installZedLoginHarness(t, "4242", "other-token", func(http.ResponseWriter, *http.Request) {})
	zedBrowserOpener = func(target string) error {
		parsed, _ := url.Parse(target)
		query := parsed.Query()
		callback := url.Values{"user_id": {"4242"}, "access_token": {encryptForZed(t, query.Get("native_app_public_key"), "stale-token", true)}}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Get("http://127.0.0.1:" + query.Get("native_app_port") + "/?" + callback.Encode())
		if err == nil {
			response.Body.Close()
		}
		return err
	}

	authDir := t.TempDir()
	if _, err := loginZed(t.Context(), authDir, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("loginZed() error = %v, want a rejection", err)
	}
	if entries, _ := os.ReadDir(authDir); len(entries) != 0 {
		t.Fatalf("a rejected sign-in left %d credential file(s) behind", len(entries))
	}
}

func TestLoginZedKeepsValidTokenWhenProfileFetchFailsTransiently(t *testing.T) {
	installZedLoginHarness(t, "99", "good-token", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	authDir := t.TempDir()
	result, err := loginZed(t.Context(), authDir, LoginOptions{})
	if err != nil {
		t.Fatalf("a transient profile failure must not discard the sign-in: %v", err)
	}
	if filepath.Base(result.Path) != "zed-99.json" {
		t.Fatalf("credential file = %s, want an identity derived from the user id", filepath.Base(result.Path))
	}
}

func TestZedAuthIsRegisteredAsPublicProvider(t *testing.T) {
	if target, err := ValidateLoginProvider("Zed"); err != nil || target != ProviderZed {
		t.Fatalf("ValidateLoginProvider(Zed) = %q, %v", target, err)
	}
	if backend, err := BackendProvider("zed"); err != nil || backend != ProviderZed {
		t.Fatalf("BackendProvider(zed) = %q, %v", backend, err)
	}
	if backend, err := normalizeCredentialBackend("zed"); err != nil || backend != ProviderZed {
		t.Fatalf("normalizeCredentialBackend(zed) = %q, %v", backend, err)
	}
}
