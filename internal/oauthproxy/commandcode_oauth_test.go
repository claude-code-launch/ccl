package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommandCodeAuthStateMatchesOfficialShape(t *testing.T) {
	first, err := commandcodeAuthState()
	if err != nil {
		t.Fatalf("commandcodeAuthState() error: %v", err)
	}
	if len(first) != 43 {
		t.Fatalf("state length = %d, want 43 (32 base64url bytes)", len(first))
	}
	if strings.ContainsAny(first, "+/=") {
		t.Fatalf("state %q contains non-base64url characters", first)
	}
	second, err := commandcodeAuthState()
	if err != nil {
		t.Fatalf("second commandcodeAuthState() error: %v", err)
	}
	if first == second {
		t.Fatal("two generated states must differ")
	}
}

func TestCommandCodeAuthURLShape(t *testing.T) {
	original := commandcodeStudioBase
	commandcodeStudioBase = "https://studio.test"
	t.Cleanup(func() { commandcodeStudioBase = original })

	if got, want := commandcodeAuthURL(5959, "st t+/?%"), "https://studio.test/studio/auth/cli?callback=http%3A%2F%2Flocalhost%3A5959%2Fcallback&state=st+t%2B%2F%3F%25"; got != want {
		t.Fatalf("auth URL = %q, want %q", got, want)
	}
}

func TestCommandCodeSanitizePastedKey(t *testing.T) {
	for raw, want := range map[string]string{
		"\x1b[200~user_key\x1b[201~": "user_key",
		"[200~user_key[201~":         "user_key",
		"  user_key  ":               "user_key",
		"user_key":                   "user_key",
	} {
		if got := commandcodeSanitizePastedKey(raw); got != want {
			t.Errorf("commandcodeSanitizePastedKey(%q) = %q, want %q", raw, got, want)
		}
	}
}

// callbackHarness runs the callback handler on a test server for the protocol
// tests below.
func commandcodeCallbackHarness(t *testing.T, state string) (*httptest.Server, <-chan commandcodeCallback, <-chan error) {
	t.Helper()
	results := make(chan commandcodeCallback, 1)
	serveErrors := make(chan error, 1)
	server := httptest.NewServer(commandcodeCallbackHandler(state, results, serveErrors))
	t.Cleanup(server.Close)
	return server, results, serveErrors
}

func commandcodePost(serverURL, body, origin string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodPost, serverURL+"/callback", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	request.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(request)
}

func TestCommandCodeCallbackHandlerAcceptsValidPost(t *testing.T) {
	server, results, serveErrors := commandcodeCallbackHarness(t, "state-abc")
	response, err := commandcodePost(server.URL, `{"apiKey":"user_cb_key","state":"state-abc","userId":"u-1","userName":"Ada","keyName":"Mac"}`, "https://commandcode.ai")
	if err != nil {
		t.Fatalf("POST /callback: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if string(raw) != `{"success":true}` {
		t.Fatalf("body = %q, want success payload", raw)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "https://commandcode.ai" {
		t.Fatalf("CORS origin = %q, want echoed whitelist entry", got)
	}
	select {
	case callback := <-results:
		if callback.apiKey != "user_cb_key" || callback.userID != "u-1" || callback.userName != "Ada" || callback.keyName != "Mac" {
			t.Fatalf("callback = %+v", callback)
		}
	case err := <-serveErrors:
		t.Fatalf("unexpected serve error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no callback delivered")
	}
}

func TestCommandCodeCallbackHandlerRejectsStateMismatch(t *testing.T) {
	server, results, _ := commandcodeCallbackHarness(t, "state-abc")
	response, err := commandcodePost(server.URL, `{"apiKey":"user_key","state":"state-evil","userId":"u-1","userName":"Ada","keyName":"Mac"}`, "")
	if err != nil {
		t.Fatalf("POST /callback: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if string(raw) != `{"success":false,"error":"Invalid state token"}` {
		t.Fatalf("body = %q, want invalid-state payload", raw)
	}
	select {
	case callback := <-results:
		t.Fatalf("state mismatch still delivered callback: %+v", callback)
	default:
	}
}

func TestCommandCodeCallbackHandlerAccessDenied(t *testing.T) {
	server, _, serveErrors := commandcodeCallbackHarness(t, "state-abc")
	response, err := commandcodePost(server.URL, `{"error":"access_denied","error_description":""}`, "")
	if err != nil {
		t.Fatalf("POST /callback: %v", err)
	}
	defer response.Body.Close()
	// The official CLI still answers success:true to the page and only then
	// surfaces the denial to the login loop.
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	select {
	case err := <-serveErrors:
		if !strings.Contains(err.Error(), "denied") {
			t.Fatalf("serve error = %v, want access-denied", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no access-denied error delivered")
	}
}

func TestCommandCodeCallbackHandlerRejectsMissingFields(t *testing.T) {
	server, _, _ := commandcodeCallbackHarness(t, "state-abc")
	response, err := commandcodePost(server.URL, `{"apiKey":"user_key"}`, "")
	if err != nil {
		t.Fatalf("POST /callback: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if string(raw) != `{"success":false,"error":"Missing required fields"}` {
		t.Fatalf("body = %q, want missing-fields payload", raw)
	}
}

func TestCommandCodeCallbackHandlerProtocolGuards(t *testing.T) {
	server, _, _ := commandcodeCallbackHarness(t, "state-abc")

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"preflight", http.MethodOptions, "/callback", http.StatusNoContent, ""},
		{"unknown path", http.MethodPost, "/other", http.StatusNotFound, `{"success":false,"error":"Not found"}`},
		{"get on callback", http.MethodGet, "/callback", http.StatusMethodNotAllowed, `{"success":false,"error":"Method not allowed. Use POST."}`},
	}
	for _, tc := range cases {
		request, err := http.NewRequest(tc.method, server.URL+tc.path, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", tc.name, err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: do: %v", tc.name, err)
		}
		defer response.Body.Close()
		if response.StatusCode != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.name, response.StatusCode, tc.wantStatus)
		}
		if got := response.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("%s: CORS fallback origin = %q, want default localhost:3000", tc.name, got)
		}
		if tc.wantBody != "" {
			raw, _ := io.ReadAll(response.Body)
			if string(raw) != tc.wantBody {
				t.Errorf("%s: body = %q, want %q", tc.name, raw, tc.wantBody)
			}
		}
	}
}

func TestLoginCommandCodeOAuthManualPastePersistsCredential(t *testing.T) {
	_, authDir := commandcodeTestHome(t)
	upstream := httptest.NewServer(commandcodeTestWhoami("user_paste_key"))
	defer upstream.Close()
	t.Setenv("COMMANDCODE_API_URL", upstream.URL)

	result, err := loginCommandCodeOAuth(context.Background(), authDir, LoginOptions{
		NoBrowser: true,
		Stdin:     strings.NewReader("  user_paste_key  \n"),
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.Provider != ProviderCommandCode || result.Backend != ProviderCommandCode {
		t.Fatalf("result = %+v", result)
	}
	raw, err := readCredentialFile(result.Path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	for key, want := range map[string]string{
		"type":      ProviderCommandCode,
		"api_key":   "user_paste_key",
		"user_id":   "manual-entry",
		"user_name": "API Key",
		"key_name":  "cli-manual-entry",
		"source":    "manual_paste",
	} {
		if raw[key] != want {
			t.Errorf("credential[%q] = %v, want %q", key, raw[key], want)
		}
	}
	if !strings.Contains(raw["authenticated_at"], "T") {
		t.Errorf("authenticated_at = %v, want ISO timestamp", raw["authenticated_at"])
	}
}

func TestLoginCommandCodeOAuthInvalidThenValidKey(t *testing.T) {
	_, authDir := commandcodeTestHome(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer user_good_key":
			writer.WriteHeader(http.StatusOK)
		default:
			http.Error(writer, `{"error":{"message":"invalid token"}}`, http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()
	t.Setenv("COMMANDCODE_API_URL", upstream.URL)

	result, err := loginCommandCodeOAuth(context.Background(), authDir, LoginOptions{
		NoBrowser: true,
		Stdin:     strings.NewReader("user_bad_key\nuser_good_key\n"),
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	raw, err := readCredentialFile(result.Path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if raw["api_key"] != "user_good_key" {
		t.Fatalf("credential api_key = %v, want the second (valid) key", raw["api_key"])
	}
}

func TestLoginCommandCodeOAuthBrowserTimeoutFallsBackToPaste(t *testing.T) {
	original := commandcodeBrowserTimeout
	commandcodeBrowserTimeout = 50 * time.Millisecond
	t.Cleanup(func() { commandcodeBrowserTimeout = original })

	_, authDir := commandcodeTestHome(t)
	upstream := httptest.NewServer(commandcodeTestWhoami("user_late_key"))
	defer upstream.Close()
	t.Setenv("COMMANDCODE_API_URL", upstream.URL)

	// The browser callback never arrives; stdin delivers the key after the
	// browser window has already expired.
	reader, writer := io.Pipe()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprintln(writer, "user_late_key")
		_ = writer.Close()
	}()
	result, err := loginCommandCodeOAuth(context.Background(), authDir, LoginOptions{
		NoBrowser: true,
		Stdin:     reader,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	raw, err := readCredentialFile(result.Path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if raw["source"] != "manual_paste" || raw["api_key"] != "user_late_key" {
		t.Fatalf("credential = %v, want late manual paste", raw)
	}
}

func TestCommandCodeSubmitKeyEmptyPastesAreRetry(t *testing.T) {
	if _, retry, err := commandcodeSubmitKey(context.Background(), "", "", "   "); err != nil || !retry {
		t.Fatalf("commandcodeSubmitKey(empty) = retry %v, err %v; want retry without error", retry, err)
	}
}

// readCredentialFile loads a saved credential into a string map for
// assertions.
func readCredentialFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		out[key] = fmt.Sprintf("%v", value)
	}
	return out, nil
}
