package oauthproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// zedZeroReader produces an endless stream of zero bytes without holding a
// buffer, so an oversized body can be assembled with one live allocation.
type zedZeroReader struct{}

func (zedZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type zedErrorReader struct{ err error }

func (r zedErrorReader) Read([]byte) (int, error) { return 0, r.err }

func zedReadCloser(reader io.Reader) io.ReadCloser { return io.NopCloser(reader) }

// TestZedRouterReportsUnreadableAndOversizedBodies covers the router's own body
// guard: a client that fails mid-body is a 400, and one that never ends is a
// 413 before any model routing happens.
func TestZedRouterReportsUnreadableAndOversizedBodies(t *testing.T) {
	router := &zedProtocolRouter{apiKey: "local-key", wires: map[string]zedWire{"m": zedWireAnthropic}}

	broken := httptest.NewRequest(http.MethodPost, "/v1/messages", zedReadCloser(zedErrorReader{err: errors.New("connection reset")}))
	broken.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	router.handleMessages(recorder, broken)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "connection reset") {
		t.Fatalf("unreadable body: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// The count path is behind the same key check and the same body guard.
	recorder = httptest.NewRecorder()
	router.handleCountTokens(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		zedReadCloser(zedErrorReader{err: errors.New("connection reset")})))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("count path without a key: status = %d, want 401", recorder.Code)
	}

	withKey := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		zedReadCloser(zedErrorReader{err: errors.New("connection reset")}))
	withKey.Header.Set("x-api-key", "local-key")
	recorder = httptest.NewRecorder()
	router.handleCountTokens(recorder, withKey)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "connection reset") {
		t.Fatalf("count path status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}

	oversized := httptest.NewRequest(http.MethodPost, "/v1/messages",
		zedReadCloser(io.LimitReader(zedZeroReader{}, zedMaxBodyBytes+1)))
	oversized.Header.Set("Authorization", "Bearer local-key")
	recorder = httptest.NewRecorder()
	router.handleMessages(recorder, oversized)
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "too large") {
		t.Fatalf("oversized body: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestZedRouterRefusesWiresWithoutAService pins the dispatch guard: a wire in
// the routing table with no data plane behind it must be refused, not nil-panic.
func TestZedRouterRefusesWiresWithoutAService(t *testing.T) {
	router := &zedProtocolRouter{apiKey: "local-key", wires: map[string]zedWire{"m": zedWireGoogle}}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	router.handleMessages(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "not routed to a supported Zed protocol") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// serveStreamed rewrites the body it was handed; a body it cannot rewrite is
	// reported before the service runs.
	recorder = httptest.NewRecorder()
	router.serveStreamed(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(`[1,2]`),
		func(http.ResponseWriter, *http.Request) { t.Error("an unrewritable body reached the service") })
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unrewritable body status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestZedRouteAliasesFillsMissingAliases(t *testing.T) {
	wires := make(map[string]zedWire)
	resolved := zedRouteAliases([]runtimeModelRoute{{Name: "claude-sonnet-test", Alias: "  "}}, zedWireAnthropic, wires)
	if len(resolved) != 1 || resolved[0].Alias != "claude-sonnet-test" {
		t.Fatalf("resolved = %+v, want the model name as the alias", resolved)
	}
	if wires["claude-sonnet-test"] != zedWireAnthropic {
		t.Fatalf("wires = %v", wires)
	}
}

// TestZedBufferedResponseFoldsAnEmptyBody covers a service that answered without
// writing anything: the fold has no message to return, which is an upstream error.
func TestZedBufferedResponseFoldsAnEmptyBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	newZedBufferedResponse().writeFolded(recorder)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestZedFoldSkipsDeltasBeforeTheMessageStarted(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_start","message":{"id":"m","content":[],"usage":{"input_tokens":2}}}`,
		"",
	}, "\n")
	message, foldErr := foldAnthropicStream([]byte(stream))
	if foldErr != nil {
		t.Fatalf("fold error = %+v", foldErr)
	}
	// The orphan delta was dropped, so the folded message carries no stop reason.
	if _, present := message["stop_reason"]; present {
		t.Fatalf("message = %v, want the pre-start delta ignored", message)
	}
	if message["usage"].(map[string]any)["input_tokens"] != float64(2) {
		t.Fatalf("usage = %v", message["usage"])
	}
}

func TestParseZedStatusRefusesValuesThatAreNeitherStringNorObject(t *testing.T) {
	for _, raw := range []string{`{"status":7}`, `{"status":null}`, `{"status":[1]}`, `{"status":true}`} {
		if line := parseZedStreamLine([]byte(raw)); line.status != "" || line.event != nil || line.failure != nil {
			t.Fatalf("%s parsed as %+v", raw, line)
		}
	}
}

func TestZedParseCloudFailureFallsBackToTheHTTPStatusText(t *testing.T) {
	failure := zedParseCloudFailure(http.StatusServiceUnavailable, nil, []byte("   "))
	if failure.message != http.StatusText(http.StatusServiceUnavailable) {
		t.Fatalf("message = %q, want the status text", failure.message)
	}
	failure = zedParseCloudFailure(http.StatusTooManyRequests, nil, nil)
	if failure.status != http.StatusTooManyRequests || failure.message != http.StatusText(http.StatusTooManyRequests) {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestZedGatewayStopIsNilSafe(t *testing.T) {
	var absent *zedGateway
	absent.Stop()
	(&zedGateway{}).Stop()
}

// TestZedGatewayReportsUnreadableAndOversizedUpstreamRequests drives the
// gateway's body guard directly: the size limit is 128 MiB, so the refusal is
// exercised through the error the reader returns rather than through a real
// over-limit upload.
func TestZedGatewayReportsUnreadableAndOversizedUpstreamRequests(t *testing.T) {
	newFakeZedCloud(t)
	gateway := startZedStubGateway(t)

	post := func(body io.Reader) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", zedReadCloser(body))
		request.Header.Set("Authorization", "Bearer "+gateway.key)
		recorder := httptest.NewRecorder()
		gateway.serveHTTP(recorder, request)
		return recorder
	}

	recorder := post(zedErrorReader{err: errors.New("connection reset")})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "read request body") {
		t.Fatalf("unreadable body: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = post(zedErrorReader{err: &http.MaxBytesError{Limit: zedMaxBodyBytes}})
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "read request body") {
		t.Fatalf("over-limit body: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestZedGatewayReportsAnUnreadableUpstreamStream covers a stream whose single
// line exceeds the scanner's buffer: the events already relayed stand, and the
// failure is reported as a terminal error event.
func TestZedGatewayReportsAnUnreadableUpstreamStream(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/completions" {
			writeTestJSON(writer, map[string]any{"token": "llm-stub"})
			return
		}
		writer.Header().Set(zedServerStatusHeader, "true")
		_, _ = io.WriteString(writer, ndjson(
			`{"event":{"type":"message_start","message":{"id":"m","content":[],"usage":{}}}}`,
			// One line larger than zedMaxStreamLine: bufio.Scanner gives up.
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("x", int(zedMaxStreamLine))+"\"}}}",
		))
	})
	gateway := startZedStubGateway(t)

	response, body := zedGatewayRequest(t, gateway, http.MethodPost, "/v1/messages", gateway.key,
		`{"model":"claude-sonnet-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "token too long") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("the events before the failure were dropped: %s", body)
	}
}

func TestZedGoogleProviderRequestRefusesBodiesItCannotRewrite(t *testing.T) {
	// A top-level array is valid JSON with no place to put a model.
	if _, err := zedGoogleProviderRequest([]byte(`[1,2]`), zedModel{ID: "g"}); err == nil {
		t.Fatal("an array body was accepted")
	}
	// A thinkingConfig that is not an object has nowhere to carry the level.
	if _, err := zedGoogleProviderRequest([]byte(`{"generationConfig":{"thinkingConfig":[]}}`), zedModel{ID: "g", SupportsThinking: true}); err == nil {
		t.Fatal("a non-object thinkingConfig was accepted")
	}
}

// TestZedSessionReportsSendFailuresAroundATokenRefresh covers doLLM's two error
// exits: the first send failing, and the resend after a refresh failing.
func TestZedSessionReportsSendFailuresAroundATokenRefresh(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/client/llm_tokens":
			writeTestJSON(writer, map[string]any{"token": "llm-fresh"})
		default:
			writeTestJSON(writer, map[string]any{"user": map[string]any{"github_login": "octo"}, "default_organization_id": zedTestOrgID})
		}
	})
	session := newZedSession(zedTestCredentialObject())
	session.organizationID, session.profileChecked = zedTestOrgID, true
	session.token, session.tokenExpiry = "llm-cached", time.Now().Add(time.Hour)
	session.client = &http.Client{Transport: zedFailingTransport{}}

	build := func(string) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, zedCloudBaseURL+"/completions", strings.NewReader("{}"))
	}
	if _, err := session.doLLM(context.Background(), build); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("first send error = %v", err)
	}

	// Now the first response rejects the token, the refresh succeeds, and the
	// resend is what fails.
	session.client = &http.Client{Transport: &zedScriptedTransport{cloud: http.DefaultTransport}}
	session.token, session.tokenExpiry = "llm-rejected", time.Now().Add(time.Hour)
	if _, err := session.doLLM(context.Background(), build); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("resend error = %v", err)
	}
}

// zedScriptedTransport rejects the first completion with an expired-token
// signal and fails the resend, while the account and token endpoints keep
// working.
type zedScriptedTransport struct {
	cloud http.RoundTripper
	mu    sync.Mutex
	sends int
}

func (t *zedScriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path != "/completions" {
		return t.cloud.RoundTrip(request)
	}
	t.mu.Lock()
	t.sends++
	first := t.sends == 1
	t.mu.Unlock()
	if !first {
		return nil, errors.New("dial tcp: connection refused")
	}
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{zedExpiredTokenHeader: []string{"true"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"expired"}`)),
		Request:    request,
	}, nil
}

// zedTruncatingCloud answers every request with headers promising more bytes
// than the body carries, so the client's read fails partway.
func zedTruncatingCloud(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack")
			return
		}
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"partial\":")
		_ = buffer.Flush()
	}))
	t.Cleanup(server.Close)
	previous := zedCloudBaseURL
	zedCloudBaseURL = server.URL
	t.Cleanup(func() { zedCloudBaseURL = previous })
	return server
}

func TestZedCloudReportsTruncatedAndUndecodableBodies(t *testing.T) {
	zedTruncatingCloud(t)
	if _, err := fetchZedProfile(context.Background(), http.DefaultClient, zedUserAuth{userID: "7", accessToken: "t"}); err == nil ||
		!strings.Contains(err.Error(), "read Zed account") {
		t.Fatalf("truncated profile error = %v", err)
	}
	session := newZedSession(zedTestCredentialObject())
	session.organizationID, session.profileChecked = zedTestOrgID, true
	// A cached token keeps discovery from minting one first, so the catalog is
	// the request that hits the truncated body.
	session.token, session.tokenExpiry = "llm-cached", time.Now().Add(time.Hour)
	if _, err := session.discoverModels(context.Background()); err == nil || !strings.Contains(err.Error(), "read Zed models") {
		t.Fatalf("truncated catalog error = %v", err)
	}

	// A complete but undecodable profile is reported as a decode failure.
	zedStubCloud(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("not json"))
	})
	if _, err := fetchZedProfile(context.Background(), http.DefaultClient, zedUserAuth{userID: "7", accessToken: "t"}); err == nil ||
		!strings.Contains(err.Error(), "decode Zed account") {
		t.Fatalf("undecodable profile error = %v", err)
	}
}

// TestZedCloudReportsRequestsItCannotBuild covers the shared request builder:
// an unusable base URL fails before anything reaches the network.
func TestZedCloudReportsRequestsItCannotBuild(t *testing.T) {
	previous := zedCloudBaseURL
	zedCloudBaseURL = "://not a url"
	t.Cleanup(func() { zedCloudBaseURL = previous })

	if _, err := newZedCloudRequest(context.Background(), http.MethodGet, "/models", nil); err == nil {
		t.Fatal("an unusable base URL built a request")
	}
	if _, err := fetchZedProfile(context.Background(), http.DefaultClient, zedUserAuth{userID: "7", accessToken: "t"}); err == nil ||
		!strings.Contains(err.Error(), "fetch Zed account") {
		t.Fatalf("profile error = %v", err)
	}
	session := newZedSession(zedTestCredentialObject())
	session.organizationID, session.profileChecked = zedTestOrgID, true
	if _, err := session.createLLMToken(context.Background(), zedTestOrgID); err == nil {
		t.Fatal("an unusable base URL minted a token")
	}
	// A cached token keeps discovery from minting first, so the catalog request
	// is the one that cannot be built.
	session.token, session.tokenExpiry = "llm-cached", time.Now().Add(time.Hour)
	if _, err := session.discoverModels(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "discover Zed models") {
		t.Fatalf("catalog error = %v", err)
	}
}

// TestZedCloudReportsUnreachableService covers the transport failures the
// session reports before it ever sees a response.
func TestZedCloudReportsUnreachableService(t *testing.T) {
	client := &http.Client{Transport: zedFailingTransport{}}
	if _, err := fetchZedProfile(context.Background(), client, zedUserAuth{userID: "7", accessToken: "t"}); err == nil ||
		!strings.Contains(err.Error(), "fetch Zed account") {
		t.Fatalf("profile error = %v", err)
	}

	session := newZedSession(zedTestCredentialObject())
	session.client = client
	session.organizationID, session.profileChecked = zedTestOrgID, true
	if _, err := session.createLLMToken(context.Background(), zedTestOrgID); err == nil ||
		!strings.Contains(err.Error(), "create Zed LLM token") {
		t.Fatalf("mint error = %v", err)
	}

	// A token the service rejects, whose replacement cannot be minted, ends the
	// request rather than looping.
	zedStubCloud(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"message":"boom"}`))
	})
	session = newZedSession(zedTestCredentialObject())
	session.organizationID, session.profileChecked = zedTestOrgID, true
	session.token, session.tokenExpiry = "llm-rejected", time.Now().Add(time.Hour)
	session.client = &http.Client{Transport: &zedScriptedTransport{cloud: http.DefaultTransport}}
	if _, err := session.doLLM(context.Background(), func(string) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, zedCloudBaseURL+"/completions", strings.NewReader("{}"))
	}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("refresh failure error = %v", err)
	}
}

func TestZedResolveOrganizationRequiresOneTheAccountReports(t *testing.T) {
	zedStubCloud(t, func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{"user": map[string]any{"github_login": "octo"}})
	})
	session := newZedSession(zedTestCredentialObject())
	if _, err := session.resolveOrganization(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "has no organization") {
		t.Fatalf("error = %v", err)
	}
}

// TestStartZedOAuthRefusesUnusableSetups covers the startup guards that run
// before any network call.
func TestStartZedOAuthRefusesUnusableSetups(t *testing.T) {
	newFakeZedCloud(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCredential := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCredential("zed-off.json", `{"type":"zed","disabled":true}`)
	writeCredential("zed-empty.json", `{"type":"zed"}`)

	if _, err := startZedOAuth(t.Context(), "", "missing.json"); err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("missing credential error = %v", err)
	}
	// A disabled credential is refused by name, not read as a sign-in.
	if _, err := startZedOAuth(t.Context(), "", "zed-off.json"); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("disabled credential error = %v", err)
	}
	if _, err := startZedOAuth(t.Context(), "", "zed-empty.json"); err == nil || !strings.Contains(err.Error(), "no user id or access token") {
		t.Fatalf("tokenless credential error = %v", err)
	}
	// Without a home directory there is no auth directory to read.
	t.Setenv("HOME", "")
	if _, err := startZedOAuth(t.Context(), "", zedTestCredent); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("homeless startup error = %v", err)
	}
}

// TestStartZedOAuthAcceptsANilParent covers callers that have no request
// context to thread: the runtime must treat it as background.
func TestStartZedOAuthAcceptsANilParent(t *testing.T) {
	cloud := newFakeZedCloud(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeZedTestCredential(t, authDir, map[string]any{
		"type": "zed", "user_id": zedTestUserID, "access_token": zedTestToken, "organization_id": zedTestOrgID,
	})

	var noContext context.Context
	runtime, err := startZedOAuth(noContext, "", zedTestCredent)
	if err != nil {
		t.Fatalf("startZedOAuth(nil, ...) error: %v", err)
	}
	t.Cleanup(runtime.Stop)
	if len(cloud.callsTo("/models")) == 0 {
		t.Fatal("the runtime never discovered the catalog")
	}
	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", true))
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("status=%d body:\n%s", response.StatusCode, body)
	}
}

func writeZedTestCredential(t *testing.T, authDir string, metadata map[string]any) {
	t.Helper()
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, zedTestCredent), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestZedGeminiReportsAnUnbuildableGatewayRequest(t *testing.T) {
	service := newZedGeminiService("local-key", &zedGateway{endpoint: "://not a url", key: "gateway-key"}, nil)
	if _, err := service.post(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("an unusable gateway endpoint built a request")
	}
}

func TestZedUserAgentIdentifiesAsZed(t *testing.T) {
	agent := zedUserAgent()
	if !strings.HasPrefix(agent, "Zed/") || !strings.Contains(agent, ";") {
		t.Fatalf("user agent = %q", agent)
	}
}

// TestZedServicesAcceptEitherCredentialSpelling pins both spellings Claude
// Code's clients use for the runtime key, on the router and on the Google
// service, and that a wrong key is refused.
func TestZedServicesAcceptEitherCredentialSpelling(t *testing.T) {
	router := &zedProtocolRouter{apiKey: "local-key"}
	service := newZedGeminiService("local-key", &zedGateway{}, nil)

	for name, header := range map[string][2]string{
		"api key":  {"x-api-key", "local-key"},
		"bearer":   {"Authorization", "Bearer local-key"},
		"spaced":   {"Authorization", "Bearer   local-key"},
		"rejected": {"x-api-key", "other-key"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		request.Header.Set(header[0], header[1])
		want := name != "rejected"
		if got := router.authorized(request); got != want {
			t.Fatalf("router %s = %t, want %t", name, got, want)
		}
		if got := service.authorized(request); got != want {
			t.Fatalf("gemini %s = %t, want %t", name, got, want)
		}
	}
}
