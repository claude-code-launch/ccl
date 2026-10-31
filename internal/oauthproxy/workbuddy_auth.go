package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	workbuddyDefaultBaseURL = "https://www.workbuddy.ai"
	// workbuddyChinaDefaultBaseURL is the mainland-China deployment of the same
	// WorkBuddy service. It runs the identical plugin API, login page and chat
	// gateway as www.workbuddy.ai; only the origin differs, so one
	// implementation serves both (see workbuddyDeployment).
	workbuddyChinaDefaultBaseURL = "https://www.workbuddy.cn"
	// workbuddyPlatform is the OAuth `platform` query parameter passed to
	// /v2/plugin/auth/state and embedded in the login URL. It selects the login
	// page brand; both deployments accept "workbuddy" (the desktop WorkBuddy
	// app's platform). The Keycloak token is minted by the "console" client no
	// matter what value is used here. The plugin API also expects the channel
	// metadata below (User-Agent + X-IDE-Type + X-Private-Data). These wire
	// fields do not establish permission for third-party subscription use.
	workbuddyPlatform = "workbuddy"
	// workbuddyClientVersion is the desktop WorkBuddy app version. It drives the
	// login URL `version` parameter, X-IDE-Version, and the first two
	// User-Agent tokens. Pinned to a real desktop 5.8.x release.
	workbuddyClientVersion = "5.8.3"
	// workbuddyChatChannel is the chat wire channel advertised in
	// X-IDE-Type/X-IDE-Name. The chat gateway rejects unrecognized channels with
	// code 11128 ("unapproved channel"); the real desktop client reports
	// "WorkBuddy" here (a product channel, distinct from the login platform).
	// Both deployments accept it: workbuddy.cn chat works with this same value,
	// so the channel is not part of what separates the two deployments.
	workbuddyChatChannel = "WorkBuddy"
	// workbuddyInternationalPlatform is the middle User-Agent token on
	// www.workbuddy.ai. The international gateway treats the CN brand
	// "WorkBuddy" as an illegal client (403/11140).
	workbuddyInternationalPlatform = "WorkBuddy AI"
	// workbuddyCLIVersion is the trailing "CLI/<ver>" token in the User-Agent,
	// matching the desktop client's three-token UA shape
	// ("WorkBuddy/<ver> <platform>/<ver> CLI/<ver>").
	workbuddyCLIVersion     = "2.179.0"
	workbuddyPendingToken   = 11217
	workbuddyPendingAccount = 12151
	// workbuddyRestrictedAccount is upstream code 11140 ("request illegal"):
	// an account-level restriction, not an expired token. Refreshing and
	// retrying it is treated as more unusual activity.
	workbuddyRestrictedAccount = 11140
	workbuddyMaxErrorBytes     = int64(1 << 20)
	workbuddyMaxJSONBytes      = int64(16 << 20)
)

// workbuddyDeployment is one public WorkBuddy deployment. Both deployments
// speak the same plugin API and chat protocol, so every request builder below
// is shared; a deployment carries only the origin its requests go to, the
// public OAuth provider name (which is also the credential `type` ccl
// persists), and the X-Domain identity derived from that origin.
type workbuddyDeployment struct {
	provider string
	baseURL  string
}

// domain is the X-Domain / token `domain` value for this deployment.
func (d workbuddyDeployment) domain() string {
	parsed, err := url.Parse(d.baseURL)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimSpace(d.baseURL)
}

var (
	// workbuddyBaseURLs maps a public OAuth provider name to its deployment
	// origin. A var rather than a constant table so tests can point either
	// deployment at an httptest server without disturbing the other.
	workbuddyBaseURLs = map[string]string{
		ProviderWorkBuddy:   workbuddyDefaultBaseURL,
		ProviderWorkBuddyCN: workbuddyChinaDefaultBaseURL,
	}
	workbuddyBrowserOpener = openBrowser
	workbuddyPollInterval  = time.Second
	workbuddyLoginTimeout  = 5 * time.Minute
	workbuddyHTTPTimeout   = 30 * time.Second
)

// workbuddyDeploymentFor resolves the deployment behind a public OAuth
// provider name.
func workbuddyDeploymentFor(providerName string) (workbuddyDeployment, error) {
	provider := strings.ToLower(strings.TrimSpace(providerName))
	baseURL, ok := workbuddyBaseURLs[provider]
	if !ok {
		return workbuddyDeployment{}, fmt.Errorf("unsupported WorkBuddy deployment %q", providerName)
	}
	return workbuddyDeployment{provider: provider, baseURL: baseURL}, nil
}

type workbuddyAPIError struct {
	Status    int
	Code      int
	Message   string
	RequestID string
}

func (e *workbuddyAPIError) Error() string {
	parts := make([]string, 0, 3)
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code %d", e.Code))
	}
	if strings.TrimSpace(e.Message) != "" {
		parts = append(parts, strings.TrimSpace(e.Message))
	}
	if strings.TrimSpace(e.RequestID) != "" {
		parts = append(parts, "request_id="+strings.TrimSpace(e.RequestID))
	}
	if len(parts) == 0 {
		return "unknown WorkBuddy API error"
	}
	return strings.Join(parts, ": ")
}

type workbuddyEnvelope[T any] struct {
	Code      int    `json:"code"`
	Message   string `json:"msg"`
	RequestID string `json:"requestId"`
	Data      T      `json:"data"`
}

type workbuddyAuthState struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

type workbuddyToken struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	TokenType        string `json:"tokenType"`
	Scope            string `json:"scope"`
	Domain           string `json:"domain"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
}

type workbuddyAccount struct {
	UID                string `json:"uid"`
	Nickname           string `json:"nickname"`
	Email              string `json:"email"`
	Type               string `json:"type"`
	EnterpriseID       string `json:"enterpriseId"`
	DepartmentFullName string `json:"departmentFullName"`
	LastLogin          bool   `json:"lastLogin"`
}

type workbuddyAccountsData struct {
	Accounts []workbuddyAccount `json:"accounts"`
}

func loginWorkBuddy(ctx context.Context, authDir, providerName string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deployment, err := workbuddyDeploymentFor(providerName)
	if err != nil {
		return LoginResult{}, err
	}
	loginCtx, cancel := context.WithTimeout(ctx, workbuddyLoginTimeout)
	defer cancel()
	client := &http.Client{Timeout: workbuddyHTTPTimeout, CheckRedirect: checkCredentialRedirect}

	stateURL := strings.TrimRight(deployment.baseURL, "/") + "/v2/plugin/auth/state?platform=" + url.QueryEscape(workbuddyPlatform)
	stateEnvelope, err := workbuddyJSON[workbuddyAuthState](loginCtx, client, http.MethodPost, stateURL, []byte(`{}`), deployment.anonymousHeaders())
	if err != nil {
		return LoginResult{}, fmt.Errorf("start WorkBuddy authorization: %w", err)
	}
	state := stateEnvelope.Data
	if strings.TrimSpace(state.State) == "" || strings.TrimSpace(state.AuthURL) == "" {
		return LoginResult{}, fmt.Errorf("start WorkBuddy authorization: incomplete response")
	}
	authURL, err := url.Parse(state.AuthURL)
	if err != nil {
		return LoginResult{}, fmt.Errorf("parse WorkBuddy authorization URL: %w", err)
	}
	query := authURL.Query()
	if query.Get("version") == "" {
		query.Set("version", workbuddyClientVersion)
	}
	authURL.RawQuery = query.Encode()
	fmt.Printf("Open %s to sign in to WorkBuddy\n", authURL.String())
	if !opts.NoBrowser {
		if openErr := workbuddyBrowserOpener(authURL.String()); openErr != nil {
			LogWarnf("WorkBuddy browser open failed; continue with the printed URL: %v", openErr)
		}
	}

	token, err := pollWorkBuddyToken(loginCtx, client, deployment, state.State)
	if err != nil {
		return LoginResult{}, err
	}
	account, err := pollWorkBuddyAccount(loginCtx, client, deployment, state.State, token)
	if err != nil {
		return LoginResult{}, err
	}
	accountsURL := strings.TrimRight(deployment.baseURL, "/") + "/v2/plugin/accounts"
	accountsEnvelope, err := workbuddyJSON[workbuddyAccountsData](loginCtx, client, http.MethodGet, accountsURL, nil, deployment.authenticatedHeaders(token, account))
	if err != nil {
		return LoginResult{}, fmt.Errorf("fetch WorkBuddy accounts: %w", err)
	}
	if len(accountsEnvelope.Data.Accounts) == 0 {
		return LoginResult{}, fmt.Errorf("fetch WorkBuddy accounts: account list is empty")
	}
	account = mergeWorkBuddyAccount(account, accountsEnvelope.Data.Accounts)
	now := time.Now()
	if token.ExpiresAt == 0 && token.ExpiresIn > 0 {
		token.ExpiresAt = now.Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli()
	}
	if token.RefreshExpiresAt == 0 && token.RefreshExpiresIn > 0 {
		token.RefreshExpiresAt = now.Add(time.Duration(token.RefreshExpiresIn) * time.Second).UnixMilli()
	}
	if strings.TrimSpace(token.Domain) == "" {
		token.Domain = deployment.domain()
	}

	metadata := map[string]any{
		"type":                 deployment.provider,
		"access_token":         token.AccessToken,
		"refresh_token":        token.RefreshToken,
		"token_type":           token.TokenType,
		"scope":                token.Scope,
		"domain":               token.Domain,
		"expires_at":           token.ExpiresAt,
		"refresh_expires_at":   token.RefreshExpiresAt,
		"user_id":              account.UID,
		"nickname":             account.Nickname,
		"email":                account.Email,
		"account_type":         account.Type,
		"enterprise_id":        account.EnterpriseID,
		"department_full_name": account.DepartmentFullName,
	}
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode WorkBuddy credential: %w", err)
	}
	raw = append(raw, '\n')
	identity := credentialIdentity(metadata, raw)
	credentialPath := filepath.Join(authDir, deployment.provider+"-"+identity+".json")
	if err := writeCredentialAtomic(credentialPath, raw); err != nil {
		return LoginResult{}, fmt.Errorf("persist WorkBuddy credential: %w", err)
	}
	return LoginResult{Provider: deployment.provider, Backend: deployment.provider, Path: credentialPath}, nil
}

func pollWorkBuddyToken(ctx context.Context, client *http.Client, deployment workbuddyDeployment, state string) (workbuddyToken, error) {
	endpoint := strings.TrimRight(deployment.baseURL, "/") + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	for {
		if err := waitWorkBuddyPoll(ctx); err != nil {
			return workbuddyToken{}, fmt.Errorf("WorkBuddy authorization: %w", err)
		}
		envelope, err := workbuddyJSON[workbuddyToken](ctx, client, http.MethodGet, endpoint, nil, deployment.anonymousHeaders())
		if err != nil {
			var apiErr *workbuddyAPIError
			if errors.As(err, &apiErr) && apiErr.Code == workbuddyPendingToken {
				continue
			}
			return workbuddyToken{}, fmt.Errorf("poll WorkBuddy token: %w", err)
		}
		if strings.TrimSpace(envelope.Data.AccessToken) == "" {
			return workbuddyToken{}, fmt.Errorf("poll WorkBuddy token: response has no access token")
		}
		return envelope.Data, nil
	}
}

func pollWorkBuddyAccount(ctx context.Context, client *http.Client, deployment workbuddyDeployment, state string, token workbuddyToken) (workbuddyAccount, error) {
	endpoint := strings.TrimRight(deployment.baseURL, "/") + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
	authFailures := 0
	for {
		if err := waitWorkBuddyPoll(ctx); err != nil {
			return workbuddyAccount{}, fmt.Errorf("WorkBuddy authorization: %w", err)
		}
		envelope, err := workbuddyJSON[workbuddyAccount](ctx, client, http.MethodGet, endpoint, nil, deployment.authenticatedHeaders(token, workbuddyAccount{}))
		if err != nil {
			var apiErr *workbuddyAPIError
			if errors.As(err, &apiErr) {
				if apiErr.Code == workbuddyPendingAccount {
					continue
				}
				if (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) && authFailures < 5 {
					authFailures++
					continue
				}
			}
			return workbuddyAccount{}, fmt.Errorf("poll WorkBuddy account: %w", err)
		}
		if strings.TrimSpace(envelope.Data.UID) == "" {
			return workbuddyAccount{}, fmt.Errorf("poll WorkBuddy account: response has no user ID")
		}
		return envelope.Data, nil
	}
}

func waitWorkBuddyPoll(ctx context.Context) error {
	timer := time.NewTimer(workbuddyPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mergeWorkBuddyAccount(current workbuddyAccount, accounts []workbuddyAccount) workbuddyAccount {
	for _, account := range accounts {
		if current.UID != "" && account.UID != current.UID {
			continue
		}
		if account.UID != "" {
			current.UID = account.UID
		}
		if account.Nickname != "" {
			current.Nickname = account.Nickname
		}
		if account.Email != "" {
			current.Email = account.Email
		}
		if account.Type != "" {
			current.Type = account.Type
		}
		if account.EnterpriseID != "" {
			current.EnterpriseID = account.EnterpriseID
		}
		if account.DepartmentFullName != "" {
			current.DepartmentFullName = account.DepartmentFullName
		}
		break
	}
	return current
}

func (d workbuddyDeployment) anonymousHeaders() http.Header {
	headers := d.clientHeaders()
	headers.Set("X-No-Authorization", "true")
	headers.Set("X-No-User-Id", "true")
	headers.Set("X-No-Enterprise-Id", "true")
	headers.Set("X-No-Department-Info", "true")
	return headers
}

func (d workbuddyDeployment) authenticatedHeaders(token workbuddyToken, account workbuddyAccount) http.Header {
	headers := d.clientHeaders()
	if token.AccessToken != "" {
		headers.Set("Authorization", "Bearer "+token.AccessToken)
	}
	if token.Domain != "" {
		headers.Set("X-Domain", token.Domain)
	}
	if account.UID != "" {
		headers.Set("X-User-Id", account.UID)
	}
	if account.EnterpriseID != "" {
		headers.Set("X-Enterprise-Id", account.EnterpriseID)
		headers.Set("X-Tenant-Id", account.EnterpriseID)
	}
	if account.DepartmentFullName != "" {
		headers.Set("X-Department-Info", account.DepartmentFullName)
	}
	return headers
}

func (d workbuddyDeployment) clientHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", d.userAgent())
	headers.Set("X-Product", "SaaS")
	headers.Set("X-Domain", d.domain())
	return headers
}

// platformLabel is the middle User-Agent token. CN uses the product
// channel; international requires a distinct brand.
func (d workbuddyDeployment) platformLabel() string {
	if d.provider == ProviderWorkBuddy {
		return workbuddyInternationalPlatform
	}
	return workbuddyChatChannel
}

func (d workbuddyDeployment) userAgent() string {
	return "WorkBuddy/" + workbuddyClientVersion + " " + d.platformLabel() + "/" + workbuddyClientVersion + " CLI/" + workbuddyCLIVersion
}

func workbuddyJSON[T any](ctx context.Context, client *http.Client, method, endpoint string, body []byte, headers http.Header) (workbuddyEnvelope[T], error) {
	var result workbuddyEnvelope[T]
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := doCredentialRequest(client, request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, workbuddyMaxJSONBytes+1))
	if err != nil {
		return result, fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > workbuddyMaxJSONBytes {
		return result, fmt.Errorf("response exceeds %d bytes", workbuddyMaxJSONBytes)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return result, &workbuddyAPIError{Status: response.StatusCode, Message: strings.TrimSpace(string(raw))}
			}
			return result, fmt.Errorf("decode response: %w", err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || result.Code != 0 {
		return result, &workbuddyAPIError{Status: response.StatusCode, Code: result.Code, Message: result.Message, RequestID: result.RequestID}
	}
	return result, nil
}
