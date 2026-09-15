package oauthproxy

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- AutoClaw's documented request signature is MD5.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	autoclawRefreshAppID       = "100003"
	autoclawRefreshAppKey      = "38d2391985e2369a5fb8227d8e6cd5e5"
	autoclawRefreshMaxBodySize = int64(1 << 20)
)

var (
	autoclawAPIOrigin = "https://autoglm-api.autoglm.ai"
	// Tests replace the endpoint; production matches AutoClaw's user API host.
	autoclawRefreshURL = autoclawAPIOrigin + "/userapi/v1/refresh"
	// AutoClaw currently falls back to agent-refresh only for business code
	// 400002. Keep the fallback configurable for deterministic tests.
	autoclawAgentRefreshURL = autoclawAPIOrigin + "/userapi/v1/agent-refresh"
)

// ImportAutoClawCredential copies the completed AutoClaw desktop login into
// ~/.ccl/auth. It is intentionally the same operation as `oauth autoclaw` so
// both command names have one source of truth and neither starts AutoClaw.
func ImportAutoClawCredential(ctx context.Context, authDir string) (LoginResult, error) {
	state, sourcePath, err := loadAutoClawDesktopAuth(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	return saveAutoClawCredential(authDir, autoClawMetadataFromDesktop(state, sourcePath))
}

type autoClawCredential struct {
	path         string
	fileName     string
	metadata     map[string]any
	accessToken  string
	refreshToken string
	deviceID     string
	version      string
	expiresAt    time.Time
	disabled     bool
}

type autoClawOAuthAuthorizer struct {
	path      string
	client    *http.Client
	version   string
	sessionID string
	agentID   string
	mu        sync.Mutex
}

func newAutoClawOAuthAuthorizer(credentialFile string) (*autoClawOAuthAuthorizer, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	fileName := filepath.Base(strings.TrimSpace(credentialFile))
	if fileName == "" || fileName == "." {
		return nil, errors.New("AutoClaw provider is not bound to a credential file; run `ccl oauth autoclaw`")
	}
	authorizer := &autoClawOAuthAuthorizer{
		path:      filepath.Join(authDir, fileName),
		sessionID: uuid.NewString(),
		agentID:   "auto-coder",
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	authorizer.version = credential.version
	return authorizer, nil
}

func (a *autoClawOAuthAuthorizer) isOAuth() bool { return true }

// decorateHeader converts the generic adapter's temporary Bearer header into
// AutoClaw's managed-proxy header contract. The route ID stays in
// X-Request-Model while normalizeAutoClawBody removes its provider prefix from
// the JSON model field.
func (a *autoClawOAuthAuthorizer) decorateHeader(header http.Header, converted *chatCompletionsConvertedRequest) {
	token := stripBearerPrefix(header.Get("Authorization"))
	header.Del("Authorization")
	// AutoClaw's desktop model broker uses fetch/undici, whose request contract
	// advertises a generic accept value even for streamed Chat Completions.
	// Keep this separate from the local Anthropic response's SSE headers.
	header.Set("Accept", "*/*")
	// Undici's default User-Agent is literally "node". The origin is fronted by
	// an Aliyun WAF that blocks Go's default Go-http-client/1.1 agent with a
	// 405 HTML block page, so the desktop app's agent must be replayed here.
	header.Set("User-Agent", "node")
	if token != "" {
		header.Set("X-Authorization", "Bearer "+token)
	}
	header.Set("X-Request-Id", uuid.NewString())
	if converted != nil {
		route := strings.TrimSpace(converted.model)
		if route == "" {
			route = strings.TrimSpace(converted.upstreamModel)
		}
		if route != "" {
			header.Set("X-Request-Model", route)
		}
	}
	header.Set("X-Client-Type", "pc")
	header.Set("X-Product", "autoclaw")
	header.Set("X-Harness-Type", "zcode")
	header.Set("X-Tm", autoClawPlatformName())
	version := strings.TrimSpace(a.version)
	if version == "" {
		version = autoClawInstalledVersion()
	}
	if version != "" {
		header.Set("X-Version", version)
	}
	header.Set("X-Lang", "zh-CN")
	header.Set("X-Channel", "official")
	header.Set("x_trace_id", "autoclaw-desktop")
	if sessionID := strings.TrimSpace(a.sessionID); sessionID != "" {
		header.Set("X-Session-Id", sessionID)
	}
	if agentID := strings.TrimSpace(a.agentID); agentID != "" {
		header.Set("X-Agent-Id", agentID)
	}
	header.Set("X-ZCode-Invocation-Id", uuid.NewString())
}

func (a *autoClawOAuthAuthorizer) authorize(ctx context.Context, forceRefresh bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return "", err
	}
	a.version = credential.version
	if credential.disabled {
		return "", fmt.Errorf("AutoClaw credential %s is disabled", credential.fileName)
	}
	if strings.TrimSpace(credential.accessToken) == "" {
		return "", fmt.Errorf("AutoClaw credential %s has no access token; run `ccl oauth autoclaw`", credential.fileName)
	}
	if forceRefresh || autoClawTokenNeedsRefresh(credential) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return "", err
		}
	}
	return credential.accessToken, nil
}

func (a *autoClawOAuthAuthorizer) load() (*autoClawCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no AutoClaw credential %s; run `ccl oauth autoclaw` first", a.fileName())
		}
		return nil, fmt.Errorf("read AutoClaw credential %s: %w", a.fileName(), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode AutoClaw credential %s: %w", a.fileName(), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(firstMetadataString(metadata, "type")))
	if credentialType != ProviderAutoClaw {
		return nil, fmt.Errorf("credential %s belongs to %q, not AutoClaw", a.fileName(), credentialType)
	}
	credential := &autoClawCredential{
		path:         a.path,
		fileName:     a.fileName(),
		metadata:     metadata,
		accessToken:  stripBearerPrefix(firstMetadataString(metadata, "access_token", "token", "zcode_token", "api_key")),
		refreshToken: firstMetadataString(metadata, "refresh_token", "refreshToken"),
		deviceID:     firstMetadataString(metadata, "device_id", "deviceId"),
		version:      firstMetadataString(metadata, "app_version", "version"),
		expiresAt:    parseAutoClawExpiry(metadata),
		disabled:     metadataBool(metadata, "disabled"),
	}
	if credential.version == "" {
		credential.version = autoclawDefaultVersion
	}
	// Credentials written by the earlier ZCode Plan integration had only a
	// bearer/api_key field. Keep those complete static credentials loadable for
	// migration, but reject partially imported refreshable sessions so a later
	// 401 does not fail with a less actionable missing-field error.
	legacyStatic := credential.refreshToken == "" && credential.deviceID == ""
	if !credential.disabled && credential.accessToken == "" {
		return nil, fmt.Errorf("AutoClaw credential %s has no access token; run `ccl oauth autoclaw` again", credential.fileName)
	}
	if !credential.disabled && !legacyStatic && (credential.refreshToken == "" || credential.deviceID == "") {
		return nil, fmt.Errorf("AutoClaw credential %s is incomplete; run `ccl oauth autoclaw` again", credential.fileName)
	}
	return credential, nil
}

func (a *autoClawOAuthAuthorizer) fileName() string { return filepath.Base(a.path) }

func (a *autoClawOAuthAuthorizer) listAuths() []*AuthInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	public := make(map[string]any, len(credential.metadata))
	for key, value := range credential.metadata {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "access_token", "token", "refresh_token", "refreshtoken", "device_id", "deviceid":
			continue
		default:
			public[key] = value
		}
	}
	return []*AuthInfo{{
		ID: credential.fileName, Provider: ProviderAutoClaw, FileName: credential.path,
		Label:  firstMetadataString(credential.metadata, "email", "name", "user_id"),
		Status: status, Disabled: credential.disabled, Metadata: public,
	}}
}

func parseAutoClawExpiry(metadata map[string]any) time.Time {
	for _, key := range []string{"expires_at", "expired", "expiry"} {
		if value := strings.TrimSpace(firstMetadataString(metadata, key)); value != "" {
			if parsed := parseCodexExpiry(value); !parsed.IsZero() {
				return parsed
			}
		}
	}
	return time.Time{}
}

func autoClawTokenNeedsRefresh(credential *autoClawCredential) bool {
	if credential == nil || strings.TrimSpace(credential.accessToken) == "" {
		return true
	}
	if credential.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(time.Minute).After(credential.expiresAt)
}

func (a *autoClawOAuthAuthorizer) refresh(ctx context.Context, credential *autoClawCredential) (*autoClawCredential, error) {
	if strings.TrimSpace(credential.refreshToken) == "" {
		return nil, fmt.Errorf("AutoClaw credential %s has no refresh token; run `ccl oauth autoclaw` again", credential.fileName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	timestamp := time.Now().Unix()
	headers := autoClawSignedHeaders(timestamp, credential.version)
	headers.Set("Authorization", "Bearer "+credential.accessToken)
	payload := map[string]string{
		"source_id":     "autoclaw",
		"device_id":     credential.deviceID,
		"refresh_token": credential.refreshToken,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	result, err := autoClawRefreshRequest(refreshCtx, a.client, autoclawRefreshURL, headers, data)
	if err != nil {
		return nil, fmt.Errorf("refresh AutoClaw token: %w", err)
	}
	if result.code == 400002 && autoclawAgentRefreshURL != "" {
		result, err = autoClawRefreshRequest(refreshCtx, a.client, autoclawAgentRefreshURL, headers, data)
		if err != nil {
			return nil, fmt.Errorf("refresh AutoClaw token through agent fallback: %w", err)
		}
	}
	if result.code != 0 {
		return nil, fmt.Errorf("refresh AutoClaw token: code %d: %s", result.code, result.message)
	}
	access, refresh := autoClawRefreshTokens(result.data)
	if access == "" {
		return nil, errors.New("refresh AutoClaw token: response has no access_token")
	}
	credential.accessToken = stripBearerPrefix(access)
	a.version = credential.version
	if refresh != "" {
		credential.refreshToken = refresh
	}
	credential.metadata["type"] = ProviderAutoClaw
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	credential.metadata["device_id"] = credential.deviceID
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if expiresAt := autoclawJWTExpiry(credential.accessToken); !expiresAt.IsZero() {
		credential.expiresAt = expiresAt
		credential.metadata["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	if err := persistAutoClawCredential(credential.path, credential.metadata); err != nil {
		return nil, fmt.Errorf("persist refreshed AutoClaw credential: %w", err)
	}
	LogInfof("credential refreshed component=autoclaw_chat credential_file=%s expires_at=%s",
		credential.fileName, credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

type autoClawRefreshResult struct {
	code    int
	message string
	data    json.RawMessage
}

func autoClawSignedHeaders(timestamp int64, versionOverride ...string) http.Header {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "*/*")
	version := ""
	if len(versionOverride) > 0 {
		version = strings.TrimSpace(versionOverride[0])
	}
	if version == "" {
		version = autoClawInstalledVersion()
	}
	header.Set("X-Version", version)
	header.Set("X-Tm", autoClawPlatformName())
	header.Set("X-Product", "autoclaw")
	header.Set("X-Auth-Appid", autoclawRefreshAppID)
	header.Set("X-Auth-TimeStamp", fmt.Sprintf("%d", timestamp))
	header.Set("X-Auth-Sign", autoClawSignature(timestamp))
	header.Set("X-Trace-Id", uuid.NewString())
	header.Set("X-Lang", "zh-CN")
	header.Set("X-Channel", "official")
	return header
}

func autoClawSignature(timestamp int64) string {
	hash := md5.Sum([]byte(fmt.Sprintf("%s&%d&%s", autoclawRefreshAppID, timestamp, autoclawRefreshAppKey))) // #nosec G401
	return hex.EncodeToString(hash[:])
}

func autoClawPlatformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "win"
	default:
		return runtime.GOOS
	}
}

func autoClawRefreshRequest(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte) (autoClawRefreshResult, error) {
	var result autoClawRefreshResult
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, autoclawRefreshMaxBodySize+1))
	if err != nil {
		return result, err
	}
	if int64(len(raw)) > autoclawRefreshMaxBodySize {
		return result, errors.New("refresh response is too large")
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return result, fmt.Errorf("decode refresh response (HTTP %d): %s", response.StatusCode, compactAutoClawBody(string(raw)))
	}
	result.code = autoClawIntCode(envelope.Code)
	result.message = strings.TrimSpace(envelope.Msg)
	if result.message == "" {
		result.message = strings.TrimSpace(envelope.Message)
	}
	result.data = envelope.Data
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fmt.Errorf("HTTP %d: %s", response.StatusCode, result.message)
	}
	return result, nil
}

func autoClawIntCode(raw json.RawMessage) int {
	var number int
	if json.Unmarshal(raw, &number) == nil {
		return number
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		_, _ = fmt.Sscanf(text, "%d", &number)
	}
	return number
}

func autoClawRefreshTokens(raw json.RawMessage) (access, refresh string) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", ""
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		return "", ""
	}
	access = autoClawJSONString(data, "access_token", "accessToken", "token")
	refresh = autoClawJSONString(data, "refresh_token", "refreshToken")
	return strings.TrimSpace(access), strings.TrimSpace(refresh)
}

func autoClawJSONString(data map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw, ok := data[key]; ok {
			var value string
			if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func compactAutoClawBody(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 200 {
		return value[:200] + "…"
	}
	return value
}

func persistAutoClawCredential(path string, metadata map[string]any) error {
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return writeCredentialAtomic(path, append(raw, '\n'))
}

// loadAutoClawCredential is kept as a small compatibility helper for doctor,
// tests, and older callers that only need the current access token.
func loadAutoClawCredential(authDir, credentialFile string) (string, map[string]any, error) {
	path := filepath.Join(authDir, filepath.Base(strings.TrimSpace(credentialFile)))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read AutoClaw credential %s: %w (run `ccl oauth autoclaw`)", filepath.Base(path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", nil, fmt.Errorf("parse AutoClaw credential %s: %w", filepath.Base(path), err)
	}
	if !strings.EqualFold(firstMetadataString(metadata, "type"), ProviderAutoClaw) {
		return "", nil, fmt.Errorf("credential %s is not an AutoClaw credential", filepath.Base(path))
	}
	access := stripBearerPrefix(firstMetadataString(metadata, "access_token", "token", "zcode_token", "api_key"))
	if access == "" {
		return "", nil, fmt.Errorf("AutoClaw credential %s has no access token; run `ccl oauth autoclaw`", filepath.Base(path))
	}
	return access, metadata, nil
}

// AutoClawAPIKey returns the current access token for compatibility with the
// generic credential inspection API. Model requests use X-Authorization and
// never expose this value to Claude Code.
func AutoClawAPIKey(credentialFile string) (string, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return "", err
	}
	access, _, err := loadAutoClawCredential(authDir, credentialFile)
	return access, err
}
