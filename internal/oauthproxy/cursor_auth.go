package oauthproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Cursor's desktop CLI sign-in is a browser approval keyed by a PKCE challenge
// pair: ccl opens the login page, the user approves in the browser, and ccl
// polls until the tokens appear. CCL owns this authenticator, its poll loop and
// its persisted credential format.

const (
	cursorLoginURL      = "https://cursor.com/loginDeepControl"
	cursorOAuthClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

	cursorPollMaxAttempts       = 150
	cursorPollMaxConsecutiveErr = 10
	cursorMaxErrorBytes         = int64(1 << 20)
)

var (
	// cursorPollURL and cursorOAuthToken are vars so tests can stub them.
	cursorPollURL    = "https://api2.cursor.sh/auth/poll"
	cursorOAuthToken = "https://api2.cursor.sh/oauth/token"

	cursorPollBaseDelay = time.Second
	cursorPollMaxDelay  = 10 * time.Second
	cursorPollBackoff   = 1.2
)

type cursorTokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

type cursorLoginParams struct {
	Verifier  string
	Challenge string
	UUID      string
	LoginURL  string
}

// cursorPKCE returns a verifier and its S256 challenge: 96 random bytes as
// unpadded base64url, which is what Cursor's CLI sends.
func cursorPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate Cursor PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// cursorLoginParamsFor builds the browser URL for one login attempt. Values are
// query-escaped, and no redirect target is supplied: the approval page is the
// only thing the user needs to see.
func cursorLoginParamsFor() (cursorLoginParams, error) {
	verifier, challenge, err := cursorPKCE()
	if err != nil {
		return cursorLoginParams{}, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return cursorLoginParams{}, fmt.Errorf("generate Cursor login UUID: %w", err)
	}
	id := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
	query := url.Values{"challenge": {challenge}, "uuid": {id}}
	return cursorLoginParams{
		Verifier: verifier, Challenge: challenge, UUID: id,
		LoginURL: cursorLoginURL + "?" + query.Encode(),
	}, nil
}

// loginCursor runs Cursor's PKCE browser login and persists the credential the
// cursorOAuthAuthorizer reads. Metadata is snake_case like every other ccl
// credential even though Cursor's poll body is camelCase.
func loginCursor(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	params, err := cursorLoginParamsFor()
	if err != nil {
		return LoginResult{}, err
	}
	fmt.Printf("Open %s and approve the sign-in\n", params.LoginURL)
	if !opts.NoBrowser {
		_ = openBrowser(params.LoginURL)
	}
	fmt.Println("Waiting for authorization...")
	client := &http.Client{Transport: newUpstreamTransport(30 * time.Second)}
	tokens, err := pollCursorTokens(ctx, client, params)
	if err != nil {
		return LoginResult{}, err
	}
	accessToken := strings.TrimSpace(tokens.AccessToken)
	if accessToken == "" {
		return LoginResult{}, fmt.Errorf("Cursor authorization returned no access token")
	}
	refreshToken := strings.TrimSpace(tokens.RefreshToken)
	sub, email := cursorJWTIdentity(accessToken)
	metadata := map[string]any{
		"type":          ProviderCursor,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"timestamp":     time.Now().UnixMilli(),
	}
	if sub != "" {
		metadata["sub"] = sub
	}
	if email != "" {
		metadata["email"] = email
	}
	if expiry := cursorTokenExpiry(accessToken); !expiry.IsZero() {
		metadata["expired"] = expiry.UTC().Format(time.RFC3339)
	}
	path := filepath.Join(authDir, cursorCredentialFileName(sub, accessToken, email))
	if err := writeCodexCredentialAtomic(path, metadata); err != nil {
		return LoginResult{}, err
	}
	if email != "" {
		fmt.Printf("Cursor authentication successful (%s)\n", email)
	} else {
		fmt.Println("Cursor authentication successful")
	}
	return LoginResult{Provider: ProviderCursor, Backend: ProviderCursor, Path: path}, nil
}

// cursorCredentialFileName is `cursor-<8 hex>.json`, keyed by the account's
// JWT subject so re-login writes the same file. Without a subject the email is
// used, and without either the access token, so two logins never collide on an
// empty identity.
func cursorCredentialFileName(sub, accessToken, email string) string {
	identity := strings.TrimSpace(sub)
	if identity == "" {
		identity = strings.TrimSpace(email)
	}
	if identity == "" {
		identity = accessToken
	}
	sum := sha256.Sum256([]byte(identity))
	return ProviderCursor + "-" + fmt.Sprintf("%x", sum[:4]) + ".json"
}

// pollCursorTokens polls until the user approves. The first attempt waits one
// interval so an immediate 404 (approval not finished) cannot burn a request,
// and a 404 only means "still pending".
func pollCursorTokens(ctx context.Context, client *http.Client, params cursorLoginParams) (cursorTokenPair, error) {
	delay := cursorPollBaseDelay
	consecutiveErrors := 0
	for attempt := 0; attempt < cursorPollMaxAttempts; attempt++ {
		if err := sleepContext(ctx, delay); err != nil {
			return cursorTokenPair{}, fmt.Errorf("Cursor authorization: %w", err)
		}
		tokens, pending, err := pollCursorOnce(ctx, client, params)
		if err != nil {
			if ctx.Err() != nil {
				return cursorTokenPair{}, fmt.Errorf("Cursor authorization: %w", ctx.Err())
			}
			consecutiveErrors++
			if consecutiveErrors >= cursorPollMaxConsecutiveErr {
				return cursorTokenPair{}, fmt.Errorf("Cursor authorization: too many consecutive poll errors (last: %v)", err)
			}
		} else {
			consecutiveErrors = 0
			if !pending {
				return tokens, nil
			}
		}
		delay = cursorNextPollDelay(delay)
	}
	return cursorTokenPair{}, fmt.Errorf("Cursor authorization timed out; run `ccl oauth cursor` again")
}

func cursorNextPollDelay(delay time.Duration) time.Duration {
	next := time.Duration(float64(delay) * cursorPollBackoff)
	if next > cursorPollMaxDelay {
		next = cursorPollMaxDelay
	}
	return next
}

// pollCursorOnce returns pending=true for 404 (approval still in progress). Any
// other non-2xx is an error, and network failures are retried by the caller.
func pollCursorOnce(ctx context.Context, client *http.Client, params cursorLoginParams) (cursorTokenPair, bool, error) {
	query := url.Values{"uuid": {params.UUID}, "verifier": {params.Verifier}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cursorPollURL+"?"+query.Encode(), nil)
	if err != nil {
		return cursorTokenPair{}, false, fmt.Errorf("create Cursor poll request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return cursorTokenPair{}, false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cursorMaxErrorBytes))
	if err != nil {
		return cursorTokenPair{}, false, fmt.Errorf("read Cursor poll response: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return cursorTokenPair{}, true, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cursorTokenPair{}, false, fmt.Errorf("Cursor poll returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokens cursorTokenPair
	if err := json.Unmarshal(body, &tokens); err != nil {
		return cursorTokenPair{}, false, fmt.Errorf("decode Cursor poll response: %w", err)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		// A 200 without a token means the approval has not landed yet.
		return cursorTokenPair{}, true, nil
	}
	return tokens, false, nil
}

// cursorJWTIdentity reads the account identity from a Cursor access token. The
// subject is authoritative; the email is a display label from either the
// Cursor-specific claim or the standard one.
func cursorJWTIdentity(token string) (sub, email string) {
	claims := parseCursorJWTClaims(token)
	if claims == nil {
		return "", ""
	}
	sub = strings.TrimSpace(claims.Sub)
	email = strings.TrimSpace(claims.CursorEmail)
	if email == "" {
		email = strings.TrimSpace(claims.Email)
	}
	return sub, email
}

type cursorJWTClaims struct {
	Sub         string  `json:"sub"`
	Email       string  `json:"email"`
	CursorEmail string  `json:"https://cursor.com/email"`
	Exp         float64 `json:"exp"`
}

func parseCursorJWTClaims(token string) *cursorJWTClaims {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := decodeBase64Flexible(parts[1])
	if err != nil {
		return nil
	}
	var claims cursorJWTClaims
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return &claims
}

// cursorTokenExpiry is the JWT expiry with the same five-minute safety margin
// Cursor's own clients apply. A zero result means the token does not state one.
func cursorTokenExpiry(token string) time.Time {
	claims := parseCursorJWTClaims(token)
	if claims == nil || claims.Exp == 0 {
		return time.Time{}
	}
	seconds := int64(claims.Exp)
	nanos := int64((claims.Exp - float64(seconds)) * 1e9)
	return time.Unix(seconds, nanos).Add(-5 * time.Minute)
}

// cursorShouldLogout reports whether a refresh failure means the credential is
// dead. Those answers are terminal: retrying or persisting anything would only
// hide the need to sign in again.
func cursorShouldLogout(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(payload.Error)) {
	case "invalid_grant", "invalid_token", "unauthorized_client":
		return true
	}
	return false
}

// cursorCredentialExists reports whether any Cursor credential is present, so a
// missing file is told apart from an unreadable one.
func cursorCredentialExists(authDir string) bool {
	matches, err := filepath.Glob(filepath.Join(authDir, ProviderCursor+"-*.json"))
	return err == nil && len(matches) > 0
}
