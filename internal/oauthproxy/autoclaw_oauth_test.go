package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestLoginAutoClawRunsBrowserOAuthWithoutDesktopState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	type capturedRequest struct {
		path    string
		version string
		body    map[string]any
	}
	requests := make(chan capturedRequest, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- capturedRequest{path: request.URL.Path, version: request.Header.Get("X-Version"), body: body}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case autoclawOAuthCaptchaConfigPath:
			_, _ = io.WriteString(writer, `{"code":0,"msg":"SUCCESS","data":{"enabled":true,"region":"ga","prefix":"sq51tr","scene_id":"18vhnjxl","captcha_supplier":"aliyun"}}`)
		case autoclawGoogleOAuthURLPath:
			_, _ = io.WriteString(writer, `{"code":0,"msg":"SUCCESS","data":{"oauth_url":"https://accounts.google.com/o/oauth2/auth?state=upstream-state"}}`)
		case autoclawGoogleOAuthLoginPath:
			_, _ = io.WriteString(writer, `{"code":0,"msg":"SUCCESS","data":{"access_token":"oauth-access-token","refresh_token":"oauth-refresh-token","user_id":"account-42","user_name":"CCL User","email":"ccl@example.com"}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(upstream.Close)
	originalOrigin := autoclawAPIOrigin
	autoclawAPIOrigin = upstream.URL
	t.Cleanup(func() { autoclawAPIOrigin = originalOrigin })

	originalOpener := autoclawBrowserOpener
	autoclawBrowserOpener = func(target string) error {
		loginURL, err := url.Parse(target)
		if err != nil {
			return err
		}
		if loginURL.Path != autoclawOAuthLoginPagePath || loginURL.Query().Get("nonce") == "" {
			t.Fatalf("browser login URL = %q", target)
		}
		pageResponse, err := http.Get(target) // #nosec G107 -- test-only loopback URL created above.
		if err != nil {
			return err
		}
		page, readErr := io.ReadAll(pageResponse.Body)
		_ = pageResponse.Body.Close()
		if readErr != nil {
			return readErr
		}
		if pageResponse.StatusCode != http.StatusOK || !bytes.Contains(page, []byte(autoclawCaptchaScriptURL)) {
			t.Fatalf("AutoClaw login page status=%d body=%q", pageResponse.StatusCode, string(page))
		}

		oauthURLRequest, err := http.NewRequest(http.MethodPost, "http://"+loginURL.Host+autoclawOAuthURLPath,
			strings.NewReader(`{"ali_captcha_verify_param":"verified-by-user"}`))
		if err != nil {
			return err
		}
		oauthURLRequest.Header.Set("Content-Type", "application/json")
		oauthURLRequest.Header.Set("X-CCL-OAuth-Nonce", loginURL.Query().Get("nonce"))
		oauthURLResponse, err := http.DefaultClient.Do(oauthURLRequest)
		if err != nil {
			return err
		}
		var start struct {
			OK       bool   `json:"ok"`
			OAuthURL string `json:"oauth_url"`
		}
		decodeErr := json.NewDecoder(oauthURLResponse.Body).Decode(&start)
		_ = oauthURLResponse.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if oauthURLResponse.StatusCode != http.StatusOK || !start.OK || !strings.HasPrefix(start.OAuthURL, "https://accounts.google.com/") {
			t.Fatalf("OAuth URL response status=%d value=%+v", oauthURLResponse.StatusCode, start)
		}

		callback := "http://" + loginURL.Host + autoclawOAuthCallbackPath + "?code=google-code&state=upstream-state"
		callbackResponse, err := http.Get(callback) // #nosec G107 -- test-only loopback URL created above.
		if err != nil {
			return err
		}
		_ = callbackResponse.Body.Close()
		if callbackResponse.StatusCode != http.StatusOK {
			t.Fatalf("callback status = %d", callbackResponse.StatusCode)
		}
		return nil
	}
	t.Cleanup(func() { autoclawBrowserOpener = originalOpener })

	authDir := t.TempDir()
	result, err := loginAutoClaw(context.Background(), authDir, LoginOptions{})
	if err != nil {
		t.Fatalf("loginAutoClaw() error: %v", err)
	}
	if result.Provider != ProviderAutoClaw || result.Backend != ProviderAutoClaw {
		t.Fatalf("login result = %+v", result)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v", info.Mode().Perm())
	}
	credential := readAutoClawTestJSON(t, result.Path)
	if credential["access_token"] != "oauth-access-token" || credential["refresh_token"] != "oauth-refresh-token" || credential["source"] != "autoclaw_oauth" {
		t.Fatalf("saved OAuth credential = %+v", credential)
	}
	if credential["user_id"] != "account-42" || credential["email"] != "ccl@example.com" {
		t.Fatalf("saved OAuth identity = %+v", credential)
	}

	captchaRequest := <-requests
	urlRequest := <-requests
	loginRequest := <-requests
	if captchaRequest.path != autoclawOAuthCaptchaConfigPath || captchaRequest.version == "" {
		t.Fatalf("captcha request = %+v", captchaRequest)
	}
	if urlRequest.path != autoclawGoogleOAuthURLPath || urlRequest.body["ali_captcha_verify_param"] != "verified-by-user" {
		t.Fatalf("OAuth URL request = %+v", urlRequest)
	}
	if urlRequest.body["source_id"] != "autoclaw" || urlRequest.body["navigate_uri"] == "" || len(urlRequest.body["device_id"].(string)) != 64 {
		t.Fatalf("OAuth URL identity = %+v", urlRequest.body)
	}
	if loginRequest.path != autoclawGoogleOAuthLoginPath || loginRequest.body["code"] != "google-code" || loginRequest.body["state"] != "upstream-state" {
		t.Fatalf("OAuth exchange request = %+v", loginRequest)
	}
	if loginRequest.body["device_id"] != urlRequest.body["device_id"] || loginRequest.body["navigate_uri"] != urlRequest.body["navigate_uri"] {
		t.Fatalf("OAuth exchange did not preserve device/callback: url=%+v login=%+v", urlRequest.body, loginRequest.body)
	}
}

func TestAutoClawOAuthURLHandlerRequiresCaptchaProof(t *testing.T) {
	handler := autoClawOAuthURLHandler(http.DefaultClient, "1.18.5", "device", "http://localhost/callback", "nonce", autoClawCaptchaConfig{Enabled: true}, &autoClawOAuthState{})
	request := httptest.NewRequest(http.MethodPost, autoclawOAuthURLPath, strings.NewReader(`{}`))
	request.Header.Set("X-CCL-OAuth-Nonce", "nonce")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "captcha verification is required") {
		t.Fatalf("missing captcha response = %d %s", response.Code, response.Body.String())
	}
}

func TestAutoClawOAuthCallbackRejectsMismatchedStateWithoutCompletingLogin(t *testing.T) {
	results := make(chan autoClawOAuthCallback, 1)
	state := &autoClawOAuthState{}
	state.set("expected-state")
	handler := autoClawOAuthCallbackHandler(results, state)

	wrong := httptest.NewRecorder()
	handler(wrong, httptest.NewRequest(http.MethodGet, autoclawOAuthCallbackPath+"?code=wrong&state=other-state", nil))
	if wrong.Code != http.StatusBadRequest || len(results) != 0 {
		t.Fatalf("mismatched callback status=%d queued=%d", wrong.Code, len(results))
	}

	right := httptest.NewRecorder()
	handler(right, httptest.NewRequest(http.MethodGet, autoclawOAuthCallbackPath+"?code=google-code&state=expected-state", nil))
	if right.Code != http.StatusOK || len(results) != 1 {
		t.Fatalf("matching callback status=%d queued=%d", right.Code, len(results))
	}
	result := <-results
	if result.code != "google-code" || result.state != "expected-state" {
		t.Fatalf("callback result = %+v", result)
	}
}
