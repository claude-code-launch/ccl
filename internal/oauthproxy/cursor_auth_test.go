package oauthproxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorLoginParamsEscapeAndOmitRedirectTarget(t *testing.T) {
	params, err := cursorLoginParamsFor()
	if err != nil {
		t.Fatal(err)
	}
	if params.Verifier == "" || params.Challenge == "" || params.UUID == "" {
		t.Fatalf("incomplete params: %+v", params)
	}
	parsed, err := url.Parse(params.LoginURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "cursor.com" || parsed.Path != "/loginDeepControl" {
		t.Fatalf("login URL = %q", params.LoginURL)
	}
	query := parsed.Query()
	if query.Get("challenge") != params.Challenge || query.Get("uuid") != params.UUID {
		t.Fatalf("query = %v want challenge=%s uuid=%s", query, params.Challenge, params.UUID)
	}
	// The dump's URL also carried a redirect target; the CLI sign-in must not.
	for _, forbidden := range []string{"redirectTarget", "mode="} {
		if strings.Contains(params.LoginURL, forbidden) {
			t.Fatalf("login URL carries %s: %s", forbidden, params.LoginURL)
		}
	}
	// The challenge must be the S256 digest of the verifier, unpadded.
	sum := sha256.Sum256([]byte(params.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); params.Challenge != want {
		t.Fatalf("challenge = %q, want S256 %q", params.Challenge, want)
	}
}

func TestPollCursorTokensWaitsBeforeFirstAttemptAndTreats404AsPending(t *testing.T) {
	oldBase, oldBackoff := cursorPollBaseDelay, cursorPollBackoff
	cursorPollBaseDelay = time.Millisecond
	cursorPollBackoff = 1
	t.Cleanup(func() { cursorPollBaseDelay, cursorPollBackoff = oldBase, oldBackoff })

	var attempts int
	var sawVerifier []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		sawVerifier = append(sawVerifier, request.URL.Query().Get("verifier"))
		if attempts < 3 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"accessToken": "a", "refreshToken": "r"})
	}))
	defer server.Close()
	oldURL := cursorPollURL
	cursorPollURL = server.URL
	t.Cleanup(func() { cursorPollURL = oldURL })

	tokens, err := pollCursorTokens(context.Background(), server.Client(), cursorLoginParams{Verifier: "verifier-1", UUID: "uuid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "a" || tokens.RefreshToken != "r" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	for _, verifier := range sawVerifier {
		if verifier != "verifier-1" {
			t.Fatalf("poll verifier = %q", verifier)
		}
	}
}

func TestPollCursorTokensGivesUpAfterConsecutiveNetworkErrors(t *testing.T) {
	oldBase, oldBackoff := cursorPollBaseDelay, cursorPollBackoff
	cursorPollBaseDelay = time.Millisecond
	cursorPollBackoff = 1
	t.Cleanup(func() { cursorPollBaseDelay, cursorPollBackoff = oldBase, oldBackoff })
	oldURL := cursorPollURL
	cursorPollURL = "http://127.0.0.1:9/auth/poll"
	t.Cleanup(func() { cursorPollURL = oldURL })

	if _, err := pollCursorTokens(context.Background(), &http.Client{Timeout: time.Second}, cursorLoginParams{UUID: "u", Verifier: "v"}); err == nil {
		t.Fatal("expected the poll loop to abort")
	} else if !strings.Contains(err.Error(), "consecutive") {
		t.Fatalf("error = %v, want the consecutive-error abort", err)
	}
}

func TestCursorCredentialFileNameUsesSubHash(t *testing.T) {
	sum := sha256.Sum256([]byte("auth0|user_1"))
	want := fmt.Sprintf("cursor-%x.json", sum[:4])
	if got := cursorCredentialFileName("auth0|user_1", "token", "a@b.c"); got != want {
		t.Fatalf("file name = %q, want %q", got, want)
	}
	// Without a subject the email identifies the account; without either the
	// token does, so two logins never collide on an empty identity.
	if got := cursorCredentialFileName("", "token", "a@b.c"); !strings.HasPrefix(got, "cursor-") || got == want {
		t.Fatalf("email-derived name = %q", got)
	}
	if got, other := cursorCredentialFileName("", "token-1", ""), cursorCredentialFileName("", "token-2", ""); got == other {
		t.Fatalf("token-derived names collide: %q", got)
	}
}

func TestCursorJWTIdentityPrecedence(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"auth0|user_1","email":"plain@example.com","https://cursor.com/email":"cursor@example.com"}`))
	token := "header." + payload + ".signature"
	sub, email := cursorJWTIdentity(token)
	if sub != "auth0|user_1" || email != "cursor@example.com" {
		t.Fatalf("identity = %q %q", sub, email)
	}

	payload = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"s","email":"plain@example.com"}`))
	if _, email := cursorJWTIdentity("h." + payload + ".s"); email != "plain@example.com" {
		t.Fatalf("fallback email = %q", email)
	}
	if sub, email := cursorJWTIdentity("not-a-jwt"); sub != "" || email != "" {
		t.Fatalf("malformed token identity = %q %q", sub, email)
	}
}

func TestCursorTokenExpiryKeepsSafetyMargin(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"exp":%d}`, expiry.Unix())))
	got := cursorTokenExpiry("h." + payload + ".s")
	if delta := expiry.Add(-5 * time.Minute).Sub(got); delta > time.Second || delta < -time.Second {
		t.Fatalf("expiry = %s, want %s", got, expiry.Add(-5*time.Minute))
	}
	if !cursorTokenExpiry("h.bad.s").IsZero() {
		t.Fatal("a token without exp must not report an expiry")
	}
}

func TestCursorShouldLogout(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnauthorized, "", true},
		{http.StatusForbidden, "", true},
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_token"}`, true},
		{http.StatusBadRequest, `{"error":"unauthorized_client"}`, true},
		{http.StatusBadRequest, `{"error":"server_error"}`, false},
		{http.StatusInternalServerError, "boom", false},
	}
	for _, tc := range cases {
		if got := cursorShouldLogout(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("cursorShouldLogout(%d, %q) = %t, want %t", tc.status, tc.body, got, tc.want)
		}
	}
}

// connectFrameForTest builds a plain Connect frame the stub server can write.
func connectFrameForTest(flags byte, payload []byte) []byte {
	return cursorFrameConnect(payload, flags)
}

func TestDecodeCursorUsableModelsToleratesShapes(t *testing.T) {
	var flat []byte
	flat = cursorAppendString(flat, 1, "composer-2")
	flat = cursorAppendString(flat, 1, "claude-4-sonnet")
	flat = cursorAppendString(flat, 1, "composer-2")
	if got := decodeCursorUsableModels(flat); len(got) != 2 || got[0] != "composer-2" {
		t.Fatalf("flat models = %v", got)
	}

	var nested []byte
	for _, id := range []string{"gpt-4o", "cursor-small"} {
		nested = cursorAppendBytes(nested, 1, cursorAppendString(nil, 1, id))
	}
	if got := decodeCursorUsableModels(nested); len(got) != 2 || got[1] != "cursor-small" {
		t.Fatalf("nested models = %v", got)
	}

	// Framing is stripped, and binary noise is rejected instead of advertised.
	framed := connectFrameForTest(0, flat)
	if got := decodeCursorUsableModels(framed); len(got) != 2 {
		t.Fatalf("framed models = %v", got)
	}
	garbage := cursorAppendBytes(nil, 1, []byte{0x00, 0x01, 0x02})
	if got := decodeCursorUsableModels(garbage); len(got) != 0 {
		t.Fatalf("garbage models = %v", got)
	}
}

func TestCursorMCPToolUsesCclProviderIdentifier(t *testing.T) {
	encoded := encodeCursorMcpToolDef(cursorMCPTool{
		Name: "Read", Description: "read a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if !strings.Contains(string(encoded), cursorMCPProviderIdentifier) || cursorMCPProviderIdentifier != "ccl" {
		t.Fatalf("provider identifier = %q", cursorMCPProviderIdentifier)
	}
	if strings.Contains(string(encoded), "proxy") {
		t.Fatal("tool definition still carries the proxy identifier")
	}
}

func TestCursorConnectEndStreamMapsStatuses(t *testing.T) {
	cases := map[string]int{
		"resource_exhausted": 429, "unauthenticated": 401, "permission_denied": 403,
		"unavailable": 503, "internal": 500, "": 502,
	}
	for code, want := range cases {
		err := cursorParseEndStream([]byte(fmt.Sprintf(`{"error":{"code":%q,"message":"x"}}`, code)))
		connectErr, ok := err.(*cursorConnectError)
		if !ok {
			t.Fatalf("code %q: error = %v", code, err)
		}
		if got := connectErr.upstreamStatus(); got != want {
			t.Errorf("code %q status = %d, want %d", code, got, want)
		}
	}
	if err := cursorParseEndStream([]byte(`{}`)); err != nil {
		t.Fatalf("empty trailer = %v", err)
	}
}

func TestCursorServiceBlobLookupAcceptsRawAndHex(t *testing.T) {
	service := &cursorService{}
	sum := sha256.Sum256([]byte("prompt"))
	service.storeBlob(sum[:], []byte(`{"role":"system"}`))
	if got := service.blob(sum[:]); string(got) != `{"role":"system"}` {
		t.Fatalf("raw lookup = %q", got)
	}
	hexID := []byte(fmt.Sprintf("%x", sum))
	if got := service.blob(hexID); string(got) != `{"role":"system"}` {
		t.Fatalf("hex lookup = %q", got)
	}
	if got := service.blob(nil); got != nil {
		t.Fatalf("empty lookup = %q", got)
	}
}

func TestCursorServiceBlobStoreIsBounded(t *testing.T) {
	service := &cursorService{}
	for index := 0; index < cursorMaxBlobs+10; index++ {
		identity := make([]byte, 32)
		binary.BigEndian.PutUint32(identity, uint32(index+1))
		service.storeBlob(identity, []byte("x"))
	}
	if len(service.blobs) != cursorMaxBlobs || len(service.blobOrder) != cursorMaxBlobs {
		t.Fatalf("store size = %d/%d, want %d", len(service.blobs), len(service.blobOrder), cursorMaxBlobs)
	}
}

func TestCursorEffectiveModelsKeepsConfiguredAliasesFirst(t *testing.T) {
	models, fallback := cursorEffectiveModels("composer-2,claude-4-sonnet[1m]", []string{"gpt-4o", "composer-2"})
	if fallback {
		t.Fatal("a discovered catalog is not a fallback")
	}
	// A "[1m]" route yields both the plain and the suffixed alias, and the
	// discovered catalog is appended once without duplicating composer-2.
	if len(models) != 4 || models[0] != "composer-2" || models[2] != "claude-4-sonnet[1m]" || models[3] != "gpt-4o" {
		t.Fatalf("models = %v", models)
	}
	models, fallback = cursorEffectiveModels("", nil)
	if !fallback || len(models) == 0 || models[0] != "composer-2" {
		t.Fatalf("fallback models = %v %t", models, fallback)
	}
	models, fallback = cursorEffectiveModels("composer-2", nil)
	if fallback || len(models) != 1 || models[0] != "composer-2" {
		t.Fatalf("configured models = %v %t", models, fallback)
	}
}

func TestConvertAnthropicToCursorSplitsHistoryAndResume(t *testing.T) {
	raw := []byte(`{
		"model":"composer-2[1m]",
		"max_tokens":1024,
		"stream":true,
		"thinking":{"type":"enabled","budget_tokens":2048},
		"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{}}}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"/tmp/a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"contents"}]}]}
		]
	}`)
	converted, err := convertAnthropicToCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if converted.model != "composer-2" || converted.clientModel != "composer-2[1m]" {
		t.Fatalf("models = %q %q", converted.model, converted.clientModel)
	}
	if !converted.thinkingEnabled {
		t.Fatal("thinking should be enabled")
	}
	if len(converted.resume) != 1 || converted.resume[0].ToolUseID != "call_1" || converted.resume[0].Text != "contents" {
		t.Fatalf("resume = %+v", converted.resume)
	}
	if len(converted.tools) != 1 || converted.tools[0].Name != "Read" {
		t.Fatalf("tools = %+v", converted.tools)
	}
	if !strings.Contains(converted.userText, "hello") || !strings.Contains(converted.userText, "TOOL_RESULT") {
		t.Fatalf("user text = %q", converted.userText)
	}
	if ids := converted.resumeToolUseIDs(); len(ids) != 1 || ids[0] != "call_1" {
		t.Fatalf("resume ids = %v", ids)
	}
}

func TestConvertAnthropicToCursorThinkingDisabled(t *testing.T) {
	raw := []byte(`{"model":"composer-2","max_tokens":16,"thinking":{"type":"disabled"},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	converted, err := convertAnthropicToCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if converted.thinkingEnabled {
		t.Fatal("an explicitly disabled thinking block must stay disabled")
	}
}

func TestCursorAuthorizerRefreshPostsJSONAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-test.json")
	if err := writeCodexCredentialAtomic(path, map[string]any{
		"type": ProviderCursor, "refresh_token": "old-refresh", "email": "a@b.c",
	}); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("refresh request = %s %s", request.Method, request.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode refresh body: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"accessToken": "new-access", "refreshToken": "new-refresh"})
	}))
	defer server.Close()
	oldURL := cursorOAuthToken
	cursorOAuthToken = server.URL
	t.Cleanup(func() { cursorOAuthToken = oldURL })

	authorizer := &cursorOAuthAuthorizer{path: path, client: server.Client()}
	token, err := authorizer.authorize(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" {
		t.Fatalf("token = %q", token)
	}
	if body["grant_type"] != "refresh_token" || body["client_id"] != cursorOAuthClientID || body["refresh_token"] != "old-refresh" {
		t.Fatalf("refresh body = %v", body)
	}
	persisted, err := authorizer.load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.accessToken != "new-access" || persisted.refreshToken != "new-refresh" {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestCursorAuthorizerRefusedRefreshDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-dead.json")
	original := map[string]any{"type": ProviderCursor, "refresh_token": "dead", "access_token": ""}
	if err := writeCodexCredentialAtomic(path, original); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	oldURL := cursorOAuthToken
	cursorOAuthToken = server.URL
	t.Cleanup(func() { cursorOAuthToken = oldURL })

	authorizer := &cursorOAuthAuthorizer{path: path, client: server.Client()}
	if _, err := authorizer.authorize(context.Background(), false); err == nil {
		t.Fatal("a refused refresh must fail")
	}
	reloaded, err := authorizer.load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.metadata["refresh_token"] != "dead" {
		t.Fatalf("credential was rewritten: %+v", reloaded.metadata)
	}
	if _, ok := reloaded.metadata["last_refresh"]; ok {
		t.Fatal("a refused refresh must not be recorded as a refresh")
	}
}

func TestCursorGateWriterHoldsEventsUntilOpen(t *testing.T) {
	recorder := httptest.NewRecorder()
	gate := &cursorGateWriter{response: recorder}
	if _, err := gate.Write([]byte("event: message_start\n\n")); err != nil {
		t.Fatal(err)
	}
	if gate.opened() || recorder.Body.Len() != 0 {
		t.Fatal("a gated writer must not release the status before the first block")
	}
	gate.open()
	if !gate.opened() || !strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("released body = %q", recorder.Body.String())
	}
	if recorder.Code != 0 && recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}
