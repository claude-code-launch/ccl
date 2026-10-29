package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type qoderCredential struct {
	path         string
	fileName     string
	accessToken  string
	refreshToken string
	userID       string
	email        string
	name         string
	machineID    string
	expiresAt    time.Time
	metadata     map[string]any
	disabled     bool
}

func (credential *qoderCredential) signingCredential() qoderSigningCredential {
	return qoderSigningCredential{
		userID:      credential.userID,
		accessToken: credential.accessToken,
		name:        credential.name,
		email:       credential.email,
		machineID:   credential.machineID,
	}
}

type qoderCredentialPool struct {
	authDir        string
	credentialFile string
	client         *http.Client
}

func newQoderCredentialPool(authDir, credentialFile string) *qoderCredentialPool {
	return &qoderCredentialPool{
		authDir:        authDir,
		credentialFile: strings.TrimSpace(credentialFile),
		client:         &http.Client{Transport: newUpstreamTransport(90 * time.Second), CheckRedirect: checkCredentialRedirect},
	}
}

func (pool *qoderCredentialPool) load() ([]*qoderCredential, error) {
	name := filepath.Base(pool.credentialFile)
	if pool.credentialFile == "" || name == "." {
		return nil, fmt.Errorf("Qoder runtime is not bound to a credential file")
	}
	credential, err := pool.loadCredential(filepath.Join(pool.authDir, name))
	if err != nil {
		return nil, err
	}
	return []*qoderCredential{credential}, nil
}

func activeQoderCredentials(credentials []*qoderCredential) []*qoderCredential {
	active := make([]*qoderCredential, 0, len(credentials))
	for _, credential := range credentials {
		if credential == nil || credential.disabled || credential.accessToken == "" {
			continue
		}
		active = append(active, credential)
	}
	return active
}

func (pool *qoderCredentialPool) ordered() ([]*qoderCredential, error) {
	credentials, err := pool.load()
	return activeQoderCredentials(credentials), err
}

func (pool *qoderCredentialPool) usable(ctx context.Context, credential *qoderCredential, force bool) (*qoderCredential, error) {
	if credential == nil {
		return nil, fmt.Errorf("Qoder credential is nil")
	}
	if credential.disabled {
		return nil, fmt.Errorf("Qoder credential %s is disabled", credential.fileName)
	}
	if !force && (credential.expiresAt.IsZero() || time.Now().Before(credential.expiresAt)) {
		return credential, nil
	}
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Qoder credential %s has expired and has no refresh token", credential.fileName)
	}

	release, err := lockOAuthCredential(ctx, credential.path)
	if err != nil {
		return nil, err
	}
	defer release()
	// Another request may have refreshed this file while we waited for the lock.
	latest, err := pool.loadCredential(credential.path)
	if err != nil {
		return nil, err
	}
	if latest.disabled {
		return nil, fmt.Errorf("Qoder credential %s is disabled", latest.fileName)
	}
	if (!force || latest.accessToken != credential.accessToken) && (latest.expiresAt.IsZero() || time.Now().Before(latest.expiresAt)) {
		return latest, nil
	}
	return pool.refresh(ctx, latest)
}

func (pool *qoderCredentialPool) loadCredential(path string) (*qoderCredential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	credentialType, _ := metadata["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(credentialType), ProviderQoder) {
		return nil, fmt.Errorf("credential %s is not a Qoder credential", filepath.Base(path))
	}
	disabled, _ := metadata["disabled"].(bool)
	credential := &qoderCredential{
		path:         path,
		fileName:     filepath.Base(path),
		accessToken:  firstMetadataString(metadata, "access_token", "token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"),
		userID:       firstMetadataString(metadata, "user_id"),
		email:        firstMetadataString(metadata, "email"),
		name:         firstMetadataString(metadata, "name", "username"),
		machineID:    firstMetadataString(metadata, "machine_id", "device_id"),
		expiresAt:    qoderMetadataExpiry(metadata["expires_at"]),
		metadata:     metadata,
		disabled:     disabled,
	}
	if (!disabled && credential.accessToken == "") || credential.userID == "" || credential.machineID == "" {
		return nil, fmt.Errorf("Qoder credential %s is missing token, user_id or machine_id", credential.fileName)
	}
	return credential, nil
}

func (pool *qoderCredentialPool) refresh(ctx context.Context, credential *qoderCredential) (*qoderCredential, error) {
	body, err := json.Marshal(map[string]string{"refreshToken": credential.refreshToken})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, qoderRefreshURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credential.accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccl")
	response, err := doCredentialRequest(pool.client, request)
	if err != nil {
		return nil, fmt.Errorf("refresh Qoder credential %s: %w", credential.fileName, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, qoderMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("refresh Qoder credential %s: read response: %w", credential.fileName, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("refresh Qoder credential %s: HTTP %d: %s", credential.fileName, response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var refreshed struct {
		Token        string          `json:"token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		ExpiresIn    int64           `json:"expires_in"`
	}
	if err := json.Unmarshal(responseBody, &refreshed); err != nil {
		return nil, fmt.Errorf("refresh Qoder credential %s: decode response: %w", credential.fileName, err)
	}
	if strings.TrimSpace(refreshed.Token) == "" {
		return nil, fmt.Errorf("refresh Qoder credential %s: response has no token", credential.fileName)
	}
	credential.accessToken = strings.TrimSpace(refreshed.Token)
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		credential.refreshToken = strings.TrimSpace(refreshed.RefreshToken)
	}
	credential.expiresAt = qoderExpiry(refreshed.ExpiresAt, refreshed.ExpiresIn)
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	credential.metadata["expires_at"] = credential.expiresAt.UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(credential.metadata)
	if err != nil {
		return nil, fmt.Errorf("refresh Qoder credential %s: encode credential: %w", credential.fileName, err)
	}
	if err := writeCredentialAtomic(credential.path, append(raw, '\n')); err != nil {
		return nil, err
	}
	LogInfoEvent("credential_refreshed", "component", "qoder", "request_id", requestLogID(ctx),
		"credential", credential.fileName)
	return credential, nil
}

func (pool *qoderCredentialPool) listAuths() []*AuthInfo {
	credentials, err := pool.load()
	if err != nil {
		return nil
	}
	auths := make([]*AuthInfo, 0, len(credentials))
	for _, credential := range credentials {
		status := StatusActive
		if credential.disabled {
			status = StatusDisabled
		}
		auths = append(auths, &AuthInfo{
			ID:       credential.fileName,
			Provider: ProviderQoder,
			FileName: credential.path,
			Label:    firstMetadataString(credential.metadata, "email", "name", "user_id"),
			Status:   status,
			Disabled: credential.disabled,
			Metadata: credential.metadata,
		})
	}
	return auths
}

func qoderMetadataExpiry(value any) time.Time {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed
		}
		if number, err := strconv.ParseInt(text, 10, 64); err == nil && number > 0 {
			return qoderUnixTime(number)
		}
	case float64:
		if typed > 0 {
			return qoderUnixTime(int64(typed))
		}
	case json.Number:
		if number, err := typed.Int64(); err == nil && number > 0 {
			return qoderUnixTime(number)
		}
	}
	return time.Time{}
}
