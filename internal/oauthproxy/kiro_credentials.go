package oauthproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const kiroTokenRefreshSkew = 5 * time.Minute

type kiroCredential struct {
	path         string
	fileName     string
	metadata     map[string]any
	accessToken  string
	refreshToken string
	profileARN   string
	authMethod   string
	provider     string
	clientID     string
	clientSecret string
	region       string
	authRegion   string
	apiRegion    string
	machineID    string
	csrfToken    string
	userID       string
	visitorID    string
	expiresAt    time.Time
	disabled     bool
}

type kiroCredentialPool struct {
	authDir         string
	credentialFiles map[string]struct{}
	restrictToFiles bool
	resolver        func() ([]string, error)
	client          *http.Client
	next            atomic.Uint64
}

var kiroCredentialRefreshLocks sync.Map

func newKiroCredentialPool(authDir string, credentialFiles []string, restrictToFiles bool, resolver func() ([]string, error)) *kiroCredentialPool {
	return &kiroCredentialPool{
		authDir:         authDir,
		credentialFiles: credentialFileSet(credentialFiles),
		restrictToFiles: restrictToFiles,
		resolver:        resolver,
		client:          &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *kiroCredentialPool) selectedFiles() (map[string]struct{}, error) {
	if p.resolver != nil {
		files, err := p.resolver()
		if err != nil {
			return nil, err
		}
		return credentialFileSet(files), nil
	}
	selected := make(map[string]struct{}, len(p.credentialFiles))
	for file := range p.credentialFiles {
		selected[file] = struct{}{}
	}
	return selected, nil
}

func (p *kiroCredentialPool) load() ([]*kiroCredential, error) {
	selected, err := p.selectedFiles()
	if err != nil {
		return nil, err
	}
	if p.restrictToFiles && len(selected) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(p.authDir)
	if err != nil {
		return nil, fmt.Errorf("read Kiro auth directory: %w", err)
	}
	credentials := make([]*kiroCredential, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		if p.restrictToFiles {
			if _, ok := selected[strings.ToLower(entry.Name())]; !ok {
				continue
			}
		}
		path := filepath.Join(p.authDir, entry.Name())
		credential, err := loadKiroCredential(path)
		if err != nil || credential.disabled {
			continue
		}
		credentials = append(credentials, credential)
	}
	sort.Slice(credentials, func(i, j int) bool {
		return strings.ToLower(credentials[i].fileName) < strings.ToLower(credentials[j].fileName)
	})
	return credentials, nil
}

func loadKiroCredential(path string) (*kiroCredential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	normalizeKiroCredentialMetadata(metadata)
	if kind := metadataString(metadata, "type"); !strings.EqualFold(kind, ProviderKiro) {
		return nil, fmt.Errorf("credential type is %q, not kiro", kind)
	}
	expiresAt := parseKiroExpiry(metadata["expires_at"])
	return &kiroCredential{
		path:         path,
		fileName:     filepath.Base(path),
		metadata:     metadata,
		accessToken:  metadataString(metadata, "access_token"),
		refreshToken: metadataString(metadata, "refresh_token"),
		profileARN:   metadataString(metadata, "profile_arn"),
		authMethod:   metadataString(metadata, "auth_method"),
		provider:     metadataString(metadata, "provider"),
		clientID:     metadataString(metadata, "client_id"),
		clientSecret: metadataString(metadata, "client_secret"),
		region:       metadataString(metadata, "region"),
		authRegion:   metadataString(metadata, "auth_region"),
		apiRegion:    metadataString(metadata, "api_region"),
		machineID:    metadataString(metadata, "machine_id"),
		csrfToken:    metadataString(metadata, "csrf_token"),
		userID:       metadataString(metadata, "user_id"),
		visitorID:    metadataString(metadata, "visitor_id"),
		expiresAt:    expiresAt,
		disabled:     metadataBool(metadata, "disabled"),
	}, nil
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func metadataBool(metadata map[string]any, key string) bool {
	value, _ := metadata[key].(bool)
	return value
}

func parseKiroExpiry(value any) time.Time {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed
		}
		if numeric, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			if numeric > 1e12 {
				return time.UnixMilli(numeric)
			}
			return time.Unix(numeric, 0)
		}
	case float64:
		if typed > 1e12 {
			return time.UnixMilli(int64(typed))
		}
		return time.Unix(int64(typed), 0)
	case json.Number:
		if numeric, err := typed.Int64(); err == nil {
			if numeric > 1e12 {
				return time.UnixMilli(numeric)
			}
			return time.Unix(numeric, 0)
		}
	}
	return time.Time{}
}

func (p *kiroCredentialPool) orderedCredentials() ([]*kiroCredential, error) {
	credentials, err := p.load()
	if err != nil || len(credentials) < 2 {
		return credentials, err
	}
	start := int((p.next.Add(1) - 1) % uint64(len(credentials)))
	ordered := append([]*kiroCredential{}, credentials[start:]...)
	ordered = append(ordered, credentials[:start]...)
	return ordered, nil
}

func (p *kiroCredentialPool) usableCredential(ctx context.Context, credential *kiroCredential, forceRefresh bool) (*kiroCredential, error) {
	lockValue, _ := kiroCredentialRefreshLocks.LoadOrStore(credential.path, &sync.Mutex{})
	refreshLock := lockValue.(*sync.Mutex)
	refreshLock.Lock()
	defer refreshLock.Unlock()

	latest, err := loadKiroCredential(credential.path)
	if err != nil {
		return nil, err
	}
	if latest.disabled {
		return nil, fmt.Errorf("Kiro credential %s is disabled", latest.fileName)
	}
	if !forceRefresh && latest.accessToken != "" &&
		(latest.expiresAt.IsZero() || time.Until(latest.expiresAt) > kiroTokenRefreshSkew) {
		return latest, nil
	}
	if latest.refreshToken == "" {
		if latest.accessToken != "" && !forceRefresh {
			return latest, nil
		}
		return nil, fmt.Errorf("Kiro credential %s has no refresh token", latest.fileName)
	}
	if err := p.refreshCredential(ctx, latest); err != nil {
		return nil, err
	}
	return loadKiroCredential(latest.path)
}

func (p *kiroCredentialPool) refreshCredential(ctx context.Context, credential *kiroCredential) error {
	region := credential.authRegion
	if region == "" {
		region = credential.region
	}
	if region == "" {
		region = kiroDefaultRegion
	}

	authMethod := strings.ToLower(strings.TrimSpace(credential.authMethod))
	useIDC := authMethod == "idc" || authMethod == "builder-id" || authMethod == "iam" ||
		(authMethod == "" && credential.clientID != "" && credential.clientSecret != "")
	var endpoint string
	var payload map[string]any
	if useIDC {
		if credential.clientID == "" || credential.clientSecret == "" {
			return fmt.Errorf("Kiro IdC credential %s is missing client_id/client_secret", credential.fileName)
		}
		endpoint = kiroOIDCEndpoint(region) + "/token"
		payload = map[string]any{
			"clientId":     credential.clientID,
			"clientSecret": credential.clientSecret,
			"refreshToken": credential.refreshToken,
			"grantType":    kiroRefreshGrantType,
		}
	} else {
		endpoint = kiroAuthEndpoint(region) + "/refreshToken"
		payload = map[string]any{"refreshToken": credential.refreshToken}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connection", "close")
	machineID := credential.effectiveMachineID()
	if useIDC {
		req.Header.Set("x-amz-user-agent", "aws-sdk-js/3.980.0 KiroIDE")
		req.Header.Set("User-Agent", "aws-sdk-js/3.980.0 ua/2.1 os/"+runtime.GOOS+" lang/js md/nodejs#22.22.0 api/sso-oidc#3.980.0 m/E KiroIDE")
		req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
		req.Header.Set("amz-sdk-request", "attempt=1; max=4")
	} else {
		req.Header.Set("User-Agent", "KiroIDE-"+kiroIDEVersion+"-"+machineID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh Kiro token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read Kiro refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("refresh Kiro token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var refreshed struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &refreshed); err != nil {
		return fmt.Errorf("decode Kiro refresh response: %w", err)
	}
	if refreshed.AccessToken == "" {
		return fmt.Errorf("refresh Kiro token: response has no access token")
	}
	credential.metadata["access_token"] = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		credential.metadata["refresh_token"] = refreshed.RefreshToken
	}
	if refreshed.ProfileARN != "" {
		credential.metadata["profile_arn"] = refreshed.ProfileARN
	}
	if refreshed.ExpiresIn > 0 {
		credential.metadata["expires_at"] = time.Now().UTC().Add(time.Duration(refreshed.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	normalized, err := json.Marshal(credential.metadata)
	if err != nil {
		return err
	}
	return writeCredentialAtomic(credential.path, append(normalized, '\n'))
}

func (p *kiroCredentialPool) listAuths() []*coreauth.Auth {
	credentials, err := p.load()
	if err != nil {
		return nil
	}
	auths := make([]*coreauth.Auth, 0, len(credentials))
	for _, credential := range credentials {
		status := coreauth.StatusActive
		auths = append(auths, &coreauth.Auth{
			ID:       credential.fileName,
			Provider: ProviderKiro,
			FileName: credential.path,
			Label:    metadataString(credential.metadata, "email"),
			Status:   status,
			Disabled: credential.disabled,
			Metadata: credential.metadata,
		})
	}
	return auths
}

func (credential *kiroCredential) effectiveAPIRegion() string {
	if credential.apiRegion != "" {
		return credential.apiRegion
	}
	if credential.region != "" {
		return credential.region
	}
	return kiroDefaultRegion
}

func (credential *kiroCredential) streamingProfileARN() string {
	if credential.profileARN != "" {
		return credential.profileARN
	}
	if strings.EqualFold(credential.authMethod, "social") ||
		strings.EqualFold(credential.provider, "github") ||
		strings.EqualFold(credential.provider, "google") {
		return kiroSocialProfileARN
	}
	return kiroBuilderProfileARN
}

func (credential *kiroCredential) effectiveMachineID() string {
	normalized := strings.ReplaceAll(strings.TrimSpace(credential.machineID), "-", "")
	if len(normalized) == 32 {
		normalized += normalized
	}
	if len(normalized) == 64 {
		if _, err := hex.DecodeString(normalized); err == nil {
			return strings.ToLower(normalized)
		}
	}
	seed := credential.refreshToken
	if seed == "" {
		seed = credential.clientID
	}
	sum := sha256.Sum256([]byte("KotlinNativeAPI/" + seed))
	return hex.EncodeToString(sum[:])
}
