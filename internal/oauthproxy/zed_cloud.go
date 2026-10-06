package oauthproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zedMaxProfileBytes = int64(4 << 20)
	// zedTokenRefreshSkew refreshes an LLM token slightly before its exp claim so
	// a request never races the expiry.
	zedTokenRefreshSkew = 30 * time.Second

	zedExpiredTokenHeader    = "x-zed-expired-token"
	zedOutdatedTokenHeader   = "x-zed-outdated-token"
	zedMinimumVersionHeader  = "x-zed-minimum-required-version"
	zedServerStatusHeader    = "x-zed-server-supports-status-messages"
	zedClientStatusHeader    = "x-zed-client-supports-status-messages"
	zedClientStreamEndHeader = "x-zed-client-supports-stream-ended-request-completion-status"
	zedClientXAIHeader       = "x-zed-client-supports-x-ai"
	zedSystemIDHeader        = "x-zed-system-id"
	zedVersionHeader         = "x-zed-version"
)

// zedUserAuth is the long-lived Zed account credential. Zed authenticates
// account endpoints with the literal header "<user_id> <access_token>".
type zedUserAuth struct {
	userID      string
	accessToken string
	systemID    string
}

func (a zedUserAuth) authorization() string { return a.userID + " " + a.accessToken }

// zedHTTPError is a non-success answer from a Zed endpoint. It implements
// upstreamStatusError so the shared retry loop can classify it.
type zedHTTPError struct {
	operation  string
	status     int
	body       string
	retryAfter string
}

func (e *zedHTTPError) Error() string {
	message := zedErrorMessage([]byte(e.body))
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.operation, e.status, message)
}

func (e *zedHTTPError) upstreamStatus() int { return e.status }

// zedErrorMessage extracts the human message from Zed's {code,message} error
// body, falling back to a bounded copy of the raw text.
func zedErrorMessage(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	var parsed struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if message := strings.TrimSpace(parsed.Message); message != "" {
			return message
		}
		if message := strings.TrimSpace(parsed.Error.Message); message != "" {
			return message
		}
	}
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	return text
}

// zedProfile is the subset of GET /client/users/me that CCL consumes.
type zedProfile struct {
	User struct {
		Username    string `json:"username"`
		GithubLogin string `json:"github_login"`
		Name        string `json:"name"`
	} `json:"user"`
	Organizations []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		IsPersonal bool   `json:"is_personal"`
	} `json:"organizations"`
	DefaultOrganizationID string `json:"default_organization_id"`
	Configuration         map[string]struct {
		IsZedModelProviderEnabled *bool `json:"is_zed_model_provider_enabled"`
	} `json:"configuration_by_organization"`
	Plan struct {
		Plan json.RawMessage `json:"plan_v3"`
	} `json:"plan"`
}

func (p *zedProfile) login() string {
	return zedFirstNonEmpty(p.User.GithubLogin, p.User.Username)
}

func (p *zedProfile) planName() string {
	var plan string
	if len(p.Plan.Plan) > 0 && json.Unmarshal(p.Plan.Plan, &plan) == nil {
		return strings.TrimSpace(plan)
	}
	return ""
}

// organizationID mirrors Zed's own choice: the account's default organization,
// else the personal one, else the first listed.
func (p *zedProfile) organizationID() string {
	if id := strings.TrimSpace(p.DefaultOrganizationID); id != "" {
		return id
	}
	for _, organization := range p.Organizations {
		if organization.IsPersonal && strings.TrimSpace(organization.ID) != "" {
			return strings.TrimSpace(organization.ID)
		}
	}
	for _, organization := range p.Organizations {
		if strings.TrimSpace(organization.ID) != "" {
			return strings.TrimSpace(organization.ID)
		}
	}
	return ""
}

// modelProviderEnabled reports whether the organization allows Zed's hosted
// models. Unknown organizations are treated as enabled.
func (p *zedProfile) modelProviderEnabled(organizationID string) bool {
	if configuration, ok := p.Configuration[organizationID]; ok && configuration.IsZedModelProviderEnabled != nil {
		return *configuration.IsZedModelProviderEnabled
	}
	return true
}

func newZedCloudRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(zedCloudBaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", zedUserAgent())
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func readZedErrorBody(response *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(response.Body, zedMaxErrorBytes))
	return strings.TrimSpace(string(body))
}

// fetchZedProfile validates the account credential and returns the account's
// organizations and plan.
func fetchZedProfile(ctx context.Context, client *http.Client, auth zedUserAuth) (*zedProfile, error) {
	request, err := newZedCloudRequest(ctx, http.MethodGet, "/client/users/me", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch Zed account: %w", err)
	}
	request.Header.Set("Authorization", auth.authorization())
	if auth.systemID != "" {
		request.Header.Set(zedSystemIDHeader, auth.systemID)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Zed account: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &zedHTTPError{operation: "fetch Zed account", status: response.StatusCode,
			body: readZedErrorBody(response), retryAfter: response.Header.Get("Retry-After")}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, zedMaxProfileBytes))
	if err != nil {
		return nil, fmt.Errorf("read Zed account: %w", err)
	}
	var profile zedProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("decode Zed account: %w", err)
	}
	return &profile, nil
}

// zedCredential is one stored Zed sign-in.
type zedCredential struct {
	path           string
	fileName       string
	auth           zedUserAuth
	login          string
	organizationID string
	metadata       map[string]any
	disabled       bool
}

func loadZedCredential(authDir, credentialFile string) (*zedCredential, error) {
	name := filepath.Base(strings.TrimSpace(credentialFile))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return nil, fmt.Errorf("Zed runtime requires a credential file; run `ccl oauth %s` first", ProviderZed)
	}
	path := filepath.Join(authDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Zed credential %s not found; run `ccl oauth %s` first", name, ProviderZed)
		}
		return nil, fmt.Errorf("read Zed credential %s: %w", name, err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Zed credential %s: %w", name, err)
	}
	if kind := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"]))); kind != ProviderZed {
		return nil, fmt.Errorf("credential %s is type %q, not Zed", name, kind)
	}
	userID := ""
	switch value := metadata["user_id"].(type) {
	case string:
		userID = strings.TrimSpace(value)
	case float64:
		userID = strconv.FormatInt(int64(value), 10)
	}
	token := firstMetadataString(metadata, "access_token")
	disabled, _ := metadata["disabled"].(bool)
	if !disabled && (userID == "" || token == "") {
		return nil, fmt.Errorf("Zed credential %s has no user id or access token; run `ccl oauth %s` again", name, ProviderZed)
	}
	return &zedCredential{
		path: path, fileName: name,
		auth:           zedUserAuth{userID: userID, accessToken: token, systemID: firstMetadataString(metadata, "system_id")},
		login:          firstMetadataString(metadata, "login", "name"),
		organizationID: firstMetadataString(metadata, "organization_id"),
		metadata:       metadata, disabled: disabled,
	}, nil
}

// zedSession turns the stored account credential into short-lived LLM tokens
// and sends authenticated requests to Zed's LLM service.
type zedSession struct {
	credential *zedCredential
	client     *http.Client

	mu             sync.Mutex
	organizationID string
	// profileChecked records that the account has been verified at least once in
	// this runtime. It is checked on first use even when a stored organization
	// exists, so plan changes and organization-level disablement surface.
	profileChecked bool
	token          string
	tokenExpiry    time.Time
}

func newZedSession(credential *zedCredential) *zedSession {
	return &zedSession{
		credential:     credential,
		organizationID: credential.organizationID,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (s *zedSession) listAuths() []*AuthInfo {
	status := StatusActive
	if s.credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID:       s.credential.fileName,
		Provider: ProviderZed,
		FileName: s.credential.path,
		Label:    zedFirstNonEmpty(s.credential.login, s.credential.auth.userID),
		Status:   status,
		Disabled: s.credential.disabled,
		Metadata: s.credential.metadata,
	}}
}

// resolveOrganization returns the organization LLM tokens are minted for,
// consulting the account profile on first use. The caller holds s.mu.
func (s *zedSession) resolveOrganization(ctx context.Context) (string, error) {
	profile, err := fetchZedProfile(ctx, s.client, s.credential.auth)
	if err != nil {
		var httpErr *zedHTTPError
		if s.organizationID != "" && !(errors.As(err, &httpErr) && httpErr.status == http.StatusUnauthorized) {
			// A stored organization is enough to keep working through a profile outage,
			// but a 401 means the credential itself is no longer valid.
			LogWarnf("Zed profile fetch failed, using stored organization credential=%s error=%v", s.credential.fileName, err)
			return s.organizationID, nil
		}
		return "", err
	}
	organizationID := profile.organizationID()
	if organizationID == "" {
		return "", fmt.Errorf("Zed account %s has no organization; open https://zed.dev/account and finish setting it up", zedFirstNonEmpty(profile.login(), s.credential.auth.userID))
	}
	if !profile.modelProviderEnabled(organizationID) {
		return "", fmt.Errorf("Zed's hosted models are disabled by your organization's configuration")
	}
	LogInfof("zed account verified credential=%s plan=%s organization=%s", s.credential.fileName, profile.planName(), organizationID)
	s.organizationID = organizationID
	return organizationID, nil
}

// llmToken returns a usable LLM token. rejected names a token the service just
// refused: if another request already replaced it the fresh one is reused,
// otherwise a new one is minted.
func (s *zedSession) llmToken(ctx context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.token != rejected &&
		(s.tokenExpiry.IsZero() || time.Now().Add(zedTokenRefreshSkew).Before(s.tokenExpiry)) {
		return s.token, nil
	}
	organizationID := s.organizationID
	if !s.profileChecked || organizationID == "" || rejected != "" {
		resolved, err := s.resolveOrganization(ctx)
		if err != nil {
			return "", err
		}
		organizationID = resolved
		s.profileChecked = true
	}
	token, err := s.createLLMToken(ctx, organizationID)
	if err != nil {
		s.token, s.tokenExpiry = "", time.Time{}
		return "", err
	}
	s.token, s.tokenExpiry = token, zedTokenExpiry(token)
	return token, nil
}

func (s *zedSession) createLLMToken(ctx context.Context, organizationID string) (string, error) {
	body, err := json.Marshal(map[string]string{"organization_id": organizationID})
	if err != nil {
		return "", err
	}
	request, err := newZedCloudRequest(ctx, http.MethodPost, "/client/llm_tokens", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", s.credential.auth.authorization())
	if s.credential.auth.systemID != "" {
		request.Header.Set(zedSystemIDHeader, s.credential.auth.systemID)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("create Zed LLM token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		failure := &zedHTTPError{operation: "create Zed LLM token", status: response.StatusCode,
			body: readZedErrorBody(response), retryAfter: response.Header.Get("Retry-After")}
		LogWarnEvent("llm_token_failed", "component", "zed", "credential", s.credential.fileName,
			"status", response.StatusCode, "error", failure)
		return "", failure
	}
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, zedMaxProfileBytes)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode Zed LLM token: %w", err)
	}
	if strings.TrimSpace(parsed.Token) == "" {
		return "", fmt.Errorf("create Zed LLM token: response has no token")
	}
	return parsed.Token, nil
}

// zedTokenExpiry reads the exp claim of a JWT-shaped LLM token. A token that
// does not parse simply never expires proactively; the service's expiry
// headers still trigger a refresh.
func zedTokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0)
}

// zedNeedsTokenRefresh reports whether the service rejected the LLM token
// itself, as opposed to rejecting the request.
func zedNeedsTokenRefresh(response *http.Response) bool {
	return response.StatusCode == http.StatusUnauthorized ||
		response.Header.Get(zedExpiredTokenHeader) != "" ||
		response.Header.Get(zedOutdatedTokenHeader) != ""
}

// doLLM sends one request authenticated with an LLM token. When the service
// signals the token is expired, outdated, or unauthorized it mints a fresh
// token and resends exactly once, matching Zed's own client. build must be
// repeatable: it runs once per attempt.
func (s *zedSession) doLLM(ctx context.Context, build func(token string) (*http.Request, error)) (*http.Response, error) {
	token, err := s.llmToken(ctx, "")
	if err != nil {
		return nil, err
	}
	response, err := s.sendLLM(build, token)
	if err != nil {
		return nil, err
	}
	if !zedNeedsTokenRefresh(response) {
		return response, nil
	}
	drainAndClose(response)
	LogWarnEvent("llm_token_rejected", "component", "zed", "request_id", requestLogID(ctx),
		"credential", s.credential.fileName, "status", response.StatusCode, "action", "refresh_and_retry_once")
	token, err = s.llmToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.sendLLM(build, token)
}

func (s *zedSession) sendLLM(build func(token string) (*http.Request, error), token string) (*http.Response, error) {
	request, err := build(token)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", zedUserAgent())
	request.Header.Set(zedVersionHeader, zedClientVersion)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Zed LLM service: %w", err)
	}
	return response, nil
}

// zedModel is one entry of GET /models.
type zedModel struct {
	Provider                  string  `json:"provider"`
	ID                        string  `json:"id"`
	DisplayName               string  `json:"display_name"`
	MaxTokenCount             int     `json:"max_token_count"`
	MaxOutputTokens           int     `json:"max_output_tokens"`
	SupportsTools             bool    `json:"supports_tools"`
	SupportsImages            bool    `json:"supports_images"`
	SupportsThinking          bool    `json:"supports_thinking"`
	SupportsFastMode          bool    `json:"supports_fast_mode"`
	SupportsParallelToolCalls bool    `json:"supports_parallel_tool_calls"`
	IsDisabled                bool    `json:"is_disabled"`
	DisabledReason            *string `json:"disabled_reason"`
}

type zedModelsResponse struct {
	Models []zedModel `json:"models"`
}

// zedRetentionConsentEnv opts in to models Zed only offers with upstream data
// retention. Zed's own client asks for explicit consent before using them, so
// CCL hides them until the user grants it the same way.
const zedRetentionConsentEnv = "CCL_ZED_ALLOW_DATA_RETENTION"

// zedRequiresDataRetention matches Anthropic's Fable family, which cannot be
// offered with zero data retention.
func zedRequiresDataRetention(model zedModel) bool {
	return strings.HasPrefix(strings.ToLower(model.ID), "claude-fable-5")
}

// zedModelWire maps a catalog entry to the wire protocol of its data plane, or
// the empty wire when CCL has no data plane for the provider.
func zedModelWire(model zedModel) (zedWire, bool) {
	switch model.Provider {
	case "anthropic":
		return zedWireAnthropic, true
	case "open_ai":
		return zedWireResponses, true
	case "x_ai":
		return zedWireChat, true
	case "google":
		return zedWireGoogle, true
	}
	return 0, false
}

// filterZedModels keeps the models CCL can serve for this account.
func filterZedModels(models []zedModel, allowDataRetention bool) []zedModel {
	kept := make([]zedModel, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		key := strings.ToLower(model.ID)
		if model.ID == "" || seen[key] || model.IsDisabled {
			continue
		}
		if _, ok := zedModelWire(model); !ok {
			continue
		}
		if zedRequiresDataRetention(model) && !allowDataRetention {
			continue
		}
		seen[key] = true
		kept = append(kept, model)
	}
	return kept
}

// discoverModels fetches the account's model catalog.
func (s *zedSession) discoverModels(ctx context.Context) ([]zedModel, error) {
	response, err := s.doLLM(ctx, func(string) (*http.Request, error) {
		request, err := newZedCloudRequest(ctx, http.MethodGet, "/models", nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set(zedClientXAIHeader, "true")
		return request, nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover Zed models: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &zedHTTPError{operation: "discover Zed models", status: response.StatusCode,
			body: readZedErrorBody(response), retryAfter: response.Header.Get("Retry-After")}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, zedMaxProfileBytes))
	if err != nil {
		return nil, fmt.Errorf("read Zed models: %w", err)
	}
	var catalog zedModelsResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode Zed models: %w", err)
	}
	models := filterZedModels(catalog.Models, strings.TrimSpace(os.Getenv(zedRetentionConsentEnv)) == "1")
	if len(models) == 0 {
		return nil, fmt.Errorf("Zed returned no usable models for this account; check your plan at https://zed.dev/account")
	}
	return models, nil
}
