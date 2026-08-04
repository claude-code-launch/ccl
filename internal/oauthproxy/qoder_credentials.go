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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
	authDir         string
	credentialFiles map[string]struct{}
	restrictToFiles bool
	resolver        func() ([]string, error)
	client          *http.Client
	next            atomic.Uint64
	refreshMu       sync.Mutex
}

func newQoderCredentialPool(authDir string, credentialFiles []string, restrictToFiles bool, resolver func() ([]string, error)) *qoderCredentialPool {
	return &qoderCredentialPool{
		authDir:         authDir,
		credentialFiles: credentialFileSet(credentialFiles),
		restrictToFiles: restrictToFiles,
		resolver:        resolver,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 90 * time.Second,
		}},
	}
}

func (pool *qoderCredentialPool) selectedFiles() (map[string]struct{}, error) {
	if pool.resolver != nil {
		files, err := pool.resolver()
		if err != nil {
			return nil, err
		}
		return credentialFileSet(files), nil
	}
	selected := make(map[string]struct{}, len(pool.credentialFiles))
	for file := range pool.credentialFiles {
		selected[file] = struct{}{}
	}
	return selected, nil
}

func (pool *qoderCredentialPool) load() ([]*qoderCredential, error) {
	selected, err := pool.selectedFiles()
	if err != nil {
		return nil, err
	}
	if pool.restrictToFiles && len(selected) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(pool.authDir)
	if err != nil {
		return nil, fmt.Errorf("read Qoder auth directory: %w", err)
	}
	credentials := make([]*qoderCredential, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		if pool.restrictToFiles {
			if _, ok := selected[strings.ToLower(entry.Name())]; !ok {
				continue
			}
		}
		path := filepath.Join(pool.authDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Qoder credential %s: %w", entry.Name(), err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			LogWarnf("skip malformed Qoder credential file %s: %v", entry.Name(), err)
			continue
		}
		credentialType, _ := metadata["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(credentialType), ProviderQoder) {
			continue
		}
		disabled, _ := metadata["disabled"].(bool)
		credential := &qoderCredential{
			path:         path,
			fileName:     entry.Name(),
			accessToken:  firstMetadataString(metadata, "access_token", "token"),
			refreshToken: firstMetadataString(metadata, "refresh_token"),
			userID:       firstMetadataString(metadata, "user_id"),
			email:        firstMetadataString(metadata, "email"),
			name:         firstMetadataString(metadata, "name", "username"),
			machineID:    firstMetadataString(metadata, "machine_id"),
			expiresAt:    qoderMetadataExpiry(metadata["expires_at"]),
			metadata:     metadata,
			disabled:     disabled,
		}
		if credential.machineID == "" {
			credential.machineID = firstMetadataString(metadata, "device_id")
		}
		if (!disabled && credential.accessToken == "") || credential.userID == "" || credential.machineID == "" {
			LogWarnf("skip Qoder credential file %s: missing token, user_id, or machine_id", entry.Name())
			continue
		}
		credentials = append(credentials, credential)
	}
	sort.Slice(credentials, func(i, j int) bool {
		return strings.ToLower(credentials[i].fileName) < strings.ToLower(credentials[j].fileName)
	})
	return credentials, nil
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
	credentials = activeQoderCredentials(credentials)
	if err != nil || len(credentials) < 2 {
		return credentials, err
	}
	start := int((pool.next.Add(1) - 1) % uint64(len(credentials)))
	ordered := append([]*qoderCredential(nil), credentials[start:]...)
	ordered = append(ordered, credentials[:start]...)
	return ordered, nil
}

func (pool *qoderCredentialPool) usable(ctx context.Context, credential *qoderCredential, force bool) (*qoderCredential, error) {
	if credential == nil {
		return nil, fmt.Errorf("Qoder credential is nil")
	}
	if !force && (credential.expiresAt.IsZero() || time.Now().Before(credential.expiresAt)) {
		return credential, nil
	}
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Qoder credential %s has expired and has no refresh token", credential.fileName)
	}

	pool.refreshMu.Lock()
	defer pool.refreshMu.Unlock()
	// Another request may have refreshed this file while we waited for the lock.
	latest, err := pool.loadCredential(credential.path)
	if err == nil && !force && (latest.expiresAt.IsZero() || time.Now().Before(latest.expiresAt)) {
		return latest, nil
	}
	if err == nil && latest.accessToken != credential.accessToken {
		credential = latest
	}
	return pool.refresh(ctx, credential)
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
	return &qoderCredential{
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
	}, nil
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
	response, err := pool.client.Do(request)
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
	LogInfof("Qoder credential refreshed file=%q", credential.fileName)
	return credential, nil
}

func (pool *qoderCredentialPool) listAuths() []*coreauth.Auth {
	credentials, err := pool.load()
	if err != nil {
		return nil
	}
	auths := make([]*coreauth.Auth, 0, len(credentials))
	for _, credential := range credentials {
		status := coreauth.StatusActive
		if credential.disabled {
			status = coreauth.StatusDisabled
		}
		auths = append(auths, &coreauth.Auth{
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
