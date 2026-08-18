package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// CredentialInfo is the non-secret state doctor reads from ~/.ccl/auth.
// Disabled / Unavailable / QuotaExceeded reflect CPA-persisted account health
// when present in the credential JSON (runtime may also keep these in memory only).
type CredentialInfo struct {
	FileName      string
	Backend       string
	Disabled      bool
	Unavailable   bool
	QuotaExceeded bool
}

// ListCredentials reads supported JSON files directly inside ~/.ccl/auth.
// Subdirectories and unrelated JSON files are ignored.
func ListCredentials() ([]CredentialInfo, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil, fmt.Errorf("read auth directory: %w", err)
	}
	credentials := make([]CredentialInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(authDir, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read credential %s: %w", entry.Name(), readErr)
		}
		credential, parseErr := parseCredential(raw)
		if parseErr != nil {
			// Keep diagnostics resilient to unrelated or partially-written files.
			continue
		}
		credential.FileName = entry.Name()
		credentials = append(credentials, credential)
	}
	sort.Slice(credentials, func(i, j int) bool {
		if credentials[i].Backend != credentials[j].Backend {
			return credentials[i].Backend < credentials[j].Backend
		}
		return strings.ToLower(credentials[i].FileName) < strings.ToLower(credentials[j].FileName)
	})
	return credentials, nil
}

func parseCredential(raw []byte) (CredentialInfo, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return CredentialInfo{}, fmt.Errorf("invalid credential JSON: %w", err)
	}
	normalizeKiroCredentialMetadata(metadata)
	rawType, _ := metadata["type"].(string)
	backend, err := normalizeCredentialBackend(rawType)
	if err != nil {
		return CredentialInfo{}, err
	}
	disabled, _ := metadata["disabled"].(bool)
	unavailable, _ := metadata["unavailable"].(bool)
	status, _ := metadata["status"].(string)
	quotaExceeded := credentialQuotaExceeded(metadata)
	if !unavailable {
		// Some CPA builds only surface unavailability via status/quota.
		if strings.EqualFold(strings.TrimSpace(status), "error") ||
			strings.EqualFold(strings.TrimSpace(status), "disabled") {
			unavailable = true
		}
	}
	return CredentialInfo{
		Backend:       backend,
		Disabled:      disabled,
		Unavailable:   unavailable,
		QuotaExceeded: quotaExceeded,
	}, nil
}

func credentialQuotaExceeded(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	if quota, ok := metadata["quota"].(map[string]any); ok {
		if exceeded, ok := quota["exceeded"].(bool); ok && exceeded {
			return true
		}
		if reason, ok := quota["reason"].(string); ok &&
			strings.Contains(strings.ToLower(reason), "quota") {
			return true
		}
	}
	if msg, ok := metadata["status_message"].(string); ok {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "quota") || strings.Contains(lower, "exhausted") {
			return true
		}
	}
	// Per-model quota blocks still count as exhausted for status summaries.
	if states, ok := metadata["model_states"].(map[string]any); ok {
		for _, raw := range states {
			state, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if q, ok := state["quota"].(map[string]any); ok {
				if exceeded, ok := q["exceeded"].(bool); ok && exceeded {
					return true
				}
			}
		}
	}
	return false
}

func normalizeCredentialBackend(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ProviderCodex, ProviderChatGPT, ProviderChatGPTLegacy:
		return ProviderCodex, nil
	case ProviderCopilot:
		return ProviderCopilot, nil
	case ProviderQoder:
		return ProviderQoder, nil
	case backendXAI, ProviderGrok:
		return backendXAI, nil
	case "antigravity", ProviderGemini:
		return "antigravity", nil
	case ProviderKimi:
		return ProviderKimi, nil
	case ProviderKiro:
		return ProviderKiro, nil
	case ProviderWorkBuddy:
		return ProviderWorkBuddy, nil
	default:
		return "", fmt.Errorf("unsupported credential type %q", value)
	}
}

func credentialIdentity(metadata map[string]any, raw []byte) string {
	keys := []string{"email", "login", "user_id", "sub", "subject", "account_id", "profile_arn", "client_id", "project_id", "device_id"}
	rawType, _ := metadata["type"].(string)
	if strings.EqualFold(strings.TrimSpace(rawType), ProviderKiro) {
		// Builder ID and Social profile ARNs are shared placeholders, so they
		// cannot identify an account. Prefer the per-login OIDC client and fall
		// back to the credential hash when Kiro does not expose an email.
		keys = []string{"email", "user_id", "sub", "subject", "account_id", "client_id", "device_id"}
	}
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok {
			if normalized := sanitizeCredentialIdentity(value); normalized != "" {
				return normalized
			}
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

// normalizeKiroCredentialMetadata accepts both ccl's snake_case credential
// shape and the camelCase token file written by Kiro IDE under
// ~/.aws/sso/cache/kiro-auth-token.json. The direct Kiro runtime consumes
// snake_case metadata, so loading normalizes it first.
func normalizeKiroCredentialMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	rawType, _ := metadata["type"].(string)
	if strings.TrimSpace(rawType) == "" && looksLikeKiroCredential(metadata) {
		metadata["type"] = ProviderKiro
		rawType = ProviderKiro
	}
	if !strings.EqualFold(strings.TrimSpace(rawType), ProviderKiro) {
		return
	}
	for camel, snake := range map[string]string{
		"accessToken":   "access_token",
		"refreshToken":  "refresh_token",
		"profileArn":    "profile_arn",
		"expiresAt":     "expires_at",
		"authMethod":    "auth_method",
		"clientId":      "client_id",
		"clientSecret":  "client_secret",
		"clientIdHash":  "client_id_hash",
		"startUrl":      "start_url",
		"authRegion":    "auth_region",
		"apiRegion":     "api_region",
		"machineId":     "machine_id",
		"tokenEndpoint": "token_endpoint",
		"issuerUrl":     "issuer_url",
		"csrfToken":     "csrf_token",
		"userId":        "user_id",
		"visitorId":     "visitor_id",
		"AccessToken":   "access_token",
		"RefreshToken":  "refresh_token",
		"Idp":           "provider",
		"UserId":        "user_id",
	} {
		if _, exists := metadata[snake]; !exists {
			if value, sourceExists := metadata[camel]; sourceExists {
				metadata[snake] = value
			}
		}
		delete(metadata, camel)
	}
}

func looksLikeKiroCredential(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	if _, lower := metadata["accessToken"]; !lower {
		if _, upper := metadata["AccessToken"]; !upper {
			return false
		}
	}
	if _, lower := metadata["refreshToken"]; !lower {
		if _, upper := metadata["RefreshToken"]; !upper {
			return false
		}
	}
	for _, key := range []string{"profileArn", "authMethod", "clientIdHash", "startUrl", "csrfToken", "Idp"} {
		if _, ok := metadata[key]; ok {
			return true
		}
	}
	return false
}

func sanitizeCredentialIdentity(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '.' || r == '_' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(strings.ToLower(b.String()), ".-_")
}

func writeCredentialAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary credential: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary credential: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary credential: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary credential: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary credential: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace credential %q: %w", filepath.Base(path), err)
	}
	return os.Chmod(path, 0o600)
}
