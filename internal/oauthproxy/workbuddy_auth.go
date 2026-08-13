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
	workbuddyPlatform       = "workbuddy-ai"
	workbuddyClientVersion  = "5.3.11"
	workbuddyPendingToken   = 11217
	workbuddyPendingAccount = 12151
	workbuddyMaxErrorBytes  = int64(1 << 20)
	workbuddyMaxJSONBytes   = int64(16 << 20)
)

var (
	workbuddyBaseURL       = workbuddyDefaultBaseURL
	workbuddyBrowserOpener = openKiroBrowser
	workbuddyPollInterval  = time.Second
	workbuddyLoginTimeout  = 5 * time.Minute
	workbuddyHTTPTimeout   = 30 * time.Second
)

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

func loginWorkBuddy(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	loginCtx, cancel := context.WithTimeout(ctx, workbuddyLoginTimeout)
	defer cancel()
	client := &http.Client{Timeout: workbuddyHTTPTimeout}

	stateURL := strings.TrimRight(workbuddyBaseURL, "/") + "/v2/plugin/auth/state?platform=" + url.QueryEscape(workbuddyPlatform)
	stateEnvelope, err := workbuddyJSON[workbuddyAuthState](loginCtx, client, http.MethodPost, stateURL, []byte(`{}`), workbuddyAnonymousHeaders())
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

	token, err := pollWorkBuddyToken(loginCtx, client, state.State)
	if err != nil {
		return LoginResult{}, err
	}
	account, err := pollWorkBuddyAccount(loginCtx, client, state.State, token)
	if err != nil {
		return LoginResult{}, err
	}
	accountsURL := strings.TrimRight(workbuddyBaseURL, "/") + "/v2/plugin/accounts"
	accountsEnvelope, err := workbuddyJSON[workbuddyAccountsData](loginCtx, client, http.MethodGet, accountsURL, nil, workbuddyAuthenticatedHeaders(token, account))
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
		token.Domain = workbuddyDomain()
	}

	metadata := map[string]any{
		"type":                 ProviderWorkBuddy,
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
	credentialPath := filepath.Join(authDir, ProviderWorkBuddy+"-"+identity+".json")
	if err := writeCredentialAtomic(credentialPath, raw); err != nil {
		return LoginResult{}, fmt.Errorf("persist WorkBuddy credential: %w", err)
	}
	return LoginResult{Provider: ProviderWorkBuddy, Backend: ProviderWorkBuddy, Path: credentialPath}, nil
}

func pollWorkBuddyToken(ctx context.Context, client *http.Client, state string) (workbuddyToken, error) {
	endpoint := strings.TrimRight(workbuddyBaseURL, "/") + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	for {
		if err := waitWorkBuddyPoll(ctx); err != nil {
			return workbuddyToken{}, fmt.Errorf("WorkBuddy authorization: %w", err)
		}
		envelope, err := workbuddyJSON[workbuddyToken](ctx, client, http.MethodGet, endpoint, nil, workbuddyAnonymousHeaders())
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

func pollWorkBuddyAccount(ctx context.Context, client *http.Client, state string, token workbuddyToken) (workbuddyAccount, error) {
	endpoint := strings.TrimRight(workbuddyBaseURL, "/") + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
	authFailures := 0
	for {
		if err := waitWorkBuddyPoll(ctx); err != nil {
			return workbuddyAccount{}, fmt.Errorf("WorkBuddy authorization: %w", err)
		}
		envelope, err := workbuddyJSON[workbuddyAccount](ctx, client, http.MethodGet, endpoint, nil, workbuddyAuthenticatedHeaders(token, workbuddyAccount{}))
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

func workbuddyAnonymousHeaders() http.Header {
	headers := workbuddyClientHeaders()
	headers.Set("X-No-Authorization", "true")
	headers.Set("X-No-User-Id", "true")
	headers.Set("X-No-Enterprise-Id", "true")
	headers.Set("X-No-Department-Info", "true")
	return headers
}

func workbuddyAuthenticatedHeaders(token workbuddyToken, account workbuddyAccount) http.Header {
	headers := workbuddyClientHeaders()
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

func workbuddyClientHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", workbuddyPlatform+"/"+workbuddyClientVersion+" "+workbuddyPlatform+"/"+workbuddyClientVersion)
	headers.Set("X-Product", "SaaS")
	headers.Set("X-Domain", workbuddyDomain())
	return headers
}

func workbuddyDomain() string {
	parsed, err := url.Parse(workbuddyBaseURL)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "www.workbuddy.ai"
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
	response, err := client.Do(request)
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
