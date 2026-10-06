package oauthproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zedStubCloud points the Zed cloud base URL at a handler for one test, for the
// session-level paths a full runtime would have to reach through many layers.
func zedStubCloud(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := zedCloudBaseURL
	zedCloudBaseURL = server.URL
	t.Cleanup(func() { zedCloudBaseURL = previous })
	return server
}

func zedTestCredentialObject() *zedCredential {
	return &zedCredential{
		path: "zed.json", fileName: "zed.json",
		auth:  zedUserAuth{userID: zedTestUserID, accessToken: zedTestToken, systemID: "sys-1"},
		login: "octo",
	}
}

func TestZedErrorMessageReadsBothSpellings(t *testing.T) {
	cases := map[string]string{
		`{"message":"top"}`:                              "top",
		`{"error":{"message":"nested"}}`:                 "nested",
		`{"message":"  spaced  "}`:                       "spaced",
		`Bad Gateway text`:                               "Bad Gateway text",
		`{"code":"x"}`:                                   `{"code":"x"}`,
		`{"error":{"message":""}}`:                       `{"error":{"message":""}}`,
		`{"message":"top","error":{"message":"nested"}}`: "top",
		"":   "",
		"  ": "",
	}
	for body, want := range cases {
		if got := zedErrorMessage([]byte(body)); got != want {
			t.Fatalf("zedErrorMessage(%q) = %q, want %q", body, got, want)
		}
	}
	// A body with nothing readable is passed through, bounded.
	long := strings.Repeat("x", 600)
	got := zedErrorMessage([]byte(long))
	if len(got) != 515 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long body = %d bytes, want a 512-byte prefix plus an ellipsis", len(got))
	}
}

func TestZedProfileReadsLoginAndToleracestUnknownPlanShape(t *testing.T) {
	var profile zedProfile
	if err := json.Unmarshal([]byte(`{"user":{"username":"octo-user"},"plan":{"plan_v3":{"tier":"pro"}}}`), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.login() != "octo-user" {
		t.Fatalf("login = %q, want the username fallback", profile.login())
	}
	// A plan that is not a plain string is unknown, not a decode failure.
	if got := profile.planName(); got != "" {
		t.Fatalf("planName = %q, want empty for a non-string plan", got)
	}
}

func TestZedCredentialLoadReportsUnreadableAndMalformedFiles(t *testing.T) {
	authDir := t.TempDir()
	// A directory named like a credential: readable path, unreadable file.
	if err := os.Mkdir(filepath.Join(authDir, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadZedCredential(authDir, "dir.json"); err == nil || !strings.Contains(err.Error(), "read Zed credential") {
		t.Fatalf("directory credential error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "bad.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadZedCredential(authDir, "bad.json"); err == nil || !strings.Contains(err.Error(), "decode Zed credential") {
		t.Fatalf("malformed credential error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "empty.json"), []byte(`{"type":"zed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadZedCredential(authDir, "empty.json"); err == nil || !strings.Contains(err.Error(), "no user id or access token") {
		t.Fatalf("tokenless credential error = %v", err)
	}
	// A disabled credential is allowed to be empty and is reported as disabled.
	if err := os.WriteFile(filepath.Join(authDir, "off.json"), []byte(`{"type":"zed","disabled":true,"name":"Octo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, err := loadZedCredential(authDir, "off.json")
	if err != nil {
		t.Fatalf("disabled credential: %v", err)
	}
	auths := newZedSession(credential).listAuths()
	if len(auths) != 1 || auths[0].Status != StatusDisabled || !auths[0].Disabled || auths[0].Label != "Octo" {
		t.Fatalf("listAuths = %+v", auths)
	}
}

func TestZedCreateLLMTokenRejectsUnusableResponses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
		status  int
	}{
		{"server error", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Retry-After", "4")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		}, "boom", http.StatusInternalServerError},
		{"empty token", func(writer http.ResponseWriter, _ *http.Request) {
			writeTestJSON(writer, map[string]any{"token": "  "})
		}, "no token", 0},
		{"not json", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte("nope"))
		}, "decode Zed LLM token", 0},
	}
	for _, tc := range cases {
		zedStubCloud(t, tc.handler)
		session := newZedSession(zedTestCredentialObject())
		_, err := session.createLLMToken(context.Background(), zedTestOrgID)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
		var httpErr *zedHTTPError
		if tc.status != 0 && (!errors.As(err, &httpErr) || httpErr.status != tc.status || httpErr.retryAfter != "4") {
			t.Fatalf("%s: error = %+v, want a %d zedHTTPError", tc.name, err, tc.status)
		}
	}
}

func TestZedDiscoverModelsReportsCatalogProblems(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"gateway failure", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(`{"message":"down"}`))
		}, "discover Zed models: HTTP 502"},
		{"not json", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte("nope"))
		}, "decode Zed models"},
		{"nothing servable", func(writer http.ResponseWriter, _ *http.Request) {
			writeTestJSON(writer, map[string]any{"models": []any{
				zedTestModel("future_ai", "mystery", nil),
				zedTestModel("anthropic", "claude-off", map[string]any{"is_disabled": true}),
			}})
		}, "no usable models"},
	}
	for _, tc := range cases {
		zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
			// Discovery authenticates with an LLM token, which the session mints
			// first; only /models is the test's business.
			if request.URL.Path != "/models" {
				writeTestJSON(writer, map[string]any{"token": "llm-stub"})
				return
			}
			tc.handler(writer, request)
		})
		session := newZedSession(zedTestCredentialObject())
		session.organizationID, session.profileChecked = zedTestOrgID, true
		if _, err := session.discoverModels(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestZedSessionClearsRejectedTokenWhenMintingFails(t *testing.T) {
	var mints int
	zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/client/llm_tokens" {
			http.NotFound(writer, request)
			return
		}
		mints++
		if mints == 1 {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
			return
		}
		writeTestJSON(writer, map[string]any{"token": "llm-fresh"})
	})

	session := newZedSession(zedTestCredentialObject())
	session.organizationID, session.profileChecked = zedTestOrgID, true
	session.token, session.tokenExpiry = "llm-stale", time.Now().Add(time.Hour)

	// A cached token the service just rejected must not be served again.
	if _, err := session.llmToken(context.Background(), "llm-stale"); err == nil {
		t.Fatal("a failed mint was reported as success")
	}
	if session.token != "" || !session.tokenExpiry.IsZero() {
		t.Fatalf("cache kept %q after a failed mint", session.token)
	}
	token, err := session.llmToken(context.Background(), "llm-stale")
	if err != nil || token != "llm-fresh" {
		t.Fatalf("second mint = %q, %v", token, err)
	}
	// The fresh token is cached: no third mint.
	if token, err := session.llmToken(context.Background(), ""); err != nil || token != "llm-fresh" || mints != 2 {
		t.Fatalf("cached token = %q, %v, mints = %d", token, err, mints)
	}
}

func TestZedSendLLMReportsTransportFailure(t *testing.T) {
	session := newZedSession(zedTestCredentialObject())
	session.client = &http.Client{Transport: zedFailingTransport{}}
	_, err := session.sendLLM(func(string) (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "http://zed.invalid/models", nil)
	}, "llm-1")
	if err == nil || !strings.Contains(err.Error(), "call Zed LLM service") {
		t.Fatalf("sendLLM error = %v", err)
	}
	// A request that cannot even be built is reported as-is.
	_, err = session.sendLLM(func(string) (*http.Request, error) {
		return nil, errors.New("no request")
	}, "llm-1")
	if err == nil || !strings.Contains(err.Error(), "no request") {
		t.Fatalf("build error = %v", err)
	}
}

type zedFailingTransport struct{}

func (zedFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func TestZedNeedsTokenRefreshReadsTheServicesSignals(t *testing.T) {
	response := func(status int, header map[string]string) *http.Response {
		built := &http.Response{StatusCode: status, Header: http.Header{}}
		for name, value := range header {
			built.Header.Set(name, value)
		}
		return built
	}
	fresh := []*http.Response{
		response(http.StatusOK, nil),
		response(http.StatusInternalServerError, nil),
		response(http.StatusTooManyRequests, nil),
	}
	for _, candidate := range fresh {
		if zedNeedsTokenRefresh(candidate) {
			t.Fatalf("status %d was read as a token rejection", candidate.StatusCode)
		}
	}
	rejected := []*http.Response{
		response(http.StatusUnauthorized, nil),
		response(http.StatusOK, map[string]string{zedExpiredTokenHeader: "true"}),
		response(http.StatusOK, map[string]string{zedOutdatedTokenHeader: "true"}),
	}
	for _, candidate := range rejected {
		if !zedNeedsTokenRefresh(candidate) {
			t.Fatalf("headers %v were not read as a token rejection", candidate.Header)
		}
	}
}

func TestZedResolveOrganizationKeepsStoredValueThroughProfileOutage(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	session := newZedSession(zedTestCredentialObject())
	session.organizationID = zedTestOrgID
	organizationID, err := session.resolveOrganization(context.Background())
	if err != nil || organizationID != zedTestOrgID {
		t.Fatalf("resolveOrganization = %q, %v; want the stored organization", organizationID, err)
	}
	// The same outage for an account that never stored one is fatal, because
	// there is nothing to mint a token against.
	bare := newZedSession(zedTestCredentialObject())
	if _, err := bare.resolveOrganization(context.Background()); err == nil {
		t.Fatal("an account with no stored organization and no profile was accepted")
	}
}
