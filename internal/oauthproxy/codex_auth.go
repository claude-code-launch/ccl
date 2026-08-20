package oauthproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	codexOAuthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	codexOAuthScope        = "openid email profile offline_access"
	codexOAuthCallbackPath = "/auth/callback"
)

type codexLoginCallback struct {
	code  string
	state string
	err   string
}

// loginCodex runs the OpenAI Codex OAuth PKCE authorization-code flow with a
// local loopback callback and persists a credential the codexOAuthAuthorizer
// reads (type/id_token/access_token/refresh_token/account_id/email/expired).
// It replaces CLIProxyAPI's codex authenticator.
func loginCodex(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	verifier, challenge, err := codexPKCE()
	if err != nil {
		return LoginResult{}, fmt.Errorf("create Codex PKCE challenge: %w", err)
	}
	state, err := randomKiroOAuthValue(16)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create Codex state: %w", err)
	}
	callbackPort := 1455
	if opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", callbackPort)))
	if err != nil {
		return LoginResult{}, fmt.Errorf("listen for Codex OAuth callback on port %d: %w", callbackPort, err)
	}
	resultCh := make(chan codexLoginCallback, 1)
	server := &http.Server{Handler: codexCallbackHandler(resultCh)}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	redirectURI := fmt.Sprintf("http://localhost:%d%s", listener.Addr().(*net.TCPAddr).Port, codexOAuthCallbackPath)
	authURL := codexOAuthAuthorizeURL + "?" + url.Values{
		"client_id":                  {codexResponsesOAuthClientID},
		"response_type":              {"code"},
		"redirect_uri":               {redirectURI},
		"scope":                      {codexOAuthScope},
		"state":                      {state},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}.Encode()
	fmt.Printf("Open %s to authorize Codex\n", authURL)
	if !opts.NoBrowser {
		_ = openBrowser(authURL)
	}
	fmt.Println("Waiting for Codex authentication callback...")

	var result codexLoginCallback
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Minute):
		return LoginResult{}, fmt.Errorf("Codex authentication timed out")
	case <-ctx.Done():
		return LoginResult{}, fmt.Errorf("Codex authentication: %w", ctx.Err())
	}
	if result.err != "" {
		return LoginResult{}, fmt.Errorf("Codex authentication failed: %s", result.err)
	}
	if result.state != state {
		return LoginResult{}, fmt.Errorf("Codex authentication failed: state mismatch")
	}
	if strings.TrimSpace(result.code) == "" {
		return LoginResult{}, fmt.Errorf("Codex authentication failed: missing authorization code")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	token, err := exchangeCodexCode(ctx, client, result.code, redirectURI, verifier)
	if err != nil {
		return LoginResult{}, err
	}
	accountID, email, planType := codexLoginJWTIdentity(token.IDToken)
	if email == "" {
		return LoginResult{}, fmt.Errorf("Codex token response missing account email")
	}
	metadata := map[string]any{
		"type":          ProviderCodex,
		"id_token":      strings.TrimSpace(token.IDToken),
		"access_token":  strings.TrimSpace(token.AccessToken),
		"refresh_token": strings.TrimSpace(token.RefreshToken),
		"email":         email,
		"last_refresh":  time.Now().UTC().Format(time.RFC3339),
	}
	if accountID != "" {
		metadata["account_id"] = accountID
	}
	if token.ExpiresIn > 0 {
		metadata["expired"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Codex credential: %w", err)
	}
	raw = append(raw, '\n')
	fileName := codexCredentialFileName(email, planType, accountID)
	path := filepath.Join(authDir, fileName)
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	fmt.Println("Codex authentication successful")
	return LoginResult{Provider: ProviderCodex, Backend: ProviderCodex, Path: path}, nil
}

func codexCallbackHandler(resultCh chan<- codexLoginCallback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(codexOAuthCallbackPath, func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		result := codexLoginCallback{
			code:  strings.TrimSpace(query.Get("code")),
			state: strings.TrimSpace(query.Get("state")),
			err:   strings.TrimSpace(query.Get("error")),
		}
		select {
		case resultCh <- result:
		default:
		}
		if result.err != "" || result.code == "" {
			writeKiroOAuthCallbackPage(writer, false)
			return
		}
		writeKiroOAuthCallbackPage(writer, true)
	})
	return mux
}

func codexPKCE() (verifier, challenge string, err error) {
	random := make([]byte, 96)
	if _, err = rand.Read(random); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(random)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

type codexLoginToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func exchangeCodexCode(ctx context.Context, client *http.Client, code, redirectURI, verifier string) (codexLoginToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexResponsesOAuthClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	if err != nil {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token codexLoginToken
	if err := json.Unmarshal(body, &token); err != nil {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: decode response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return codexLoginToken{}, fmt.Errorf("exchange Codex authorization code: response missing access_token")
	}
	return token, nil
}

// codexLoginJWTIdentity decodes the id_token payload to recover the account
// email, chatgpt account id, and plan type used for credential naming.
func codexLoginJWTIdentity(token string) (accountID, email, planType string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", ""
	}
	return strings.TrimSpace(claims.Auth.AccountID), strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Auth.PlanType)
}

// codexCredentialFileName mirrors CPA's Codex CredentialFileName: it includes a
// short account hash and plan type when available to keep same-email accounts
// distinct, falling back to the email-only format.
func codexCredentialFileName(email, planType, accountID string) string {
	email = sanitizeCredentialIdentity(email)
	plan := strings.ToLower(strings.TrimSpace(planType))
	if accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		hash := hex.EncodeToString(digest[:])[:8]
		switch {
		case plan == "" && email == "":
			return fmt.Sprintf("%s-%s.json", ProviderCodex, hash)
		case plan == "":
			return fmt.Sprintf("%s-%s-%s.json", ProviderCodex, hash, email)
		case email == "":
			return fmt.Sprintf("%s-%s-%s.json", ProviderCodex, hash, plan)
		default:
			return fmt.Sprintf("%s-%s-%s-%s.json", ProviderCodex, hash, email, plan)
		}
	}
	switch {
	case plan == "" && email == "":
		return ProviderCodex + "-" + fmt.Sprintf("%d", time.Now().UnixMilli()) + ".json"
	case plan == "":
		return fmt.Sprintf("%s-%s.json", ProviderCodex, email)
	case email == "":
		return fmt.Sprintf("%s-%s.json", ProviderCodex, plan)
	default:
		return fmt.Sprintf("%s-%s-%s.json", ProviderCodex, email, plan)
	}
}
