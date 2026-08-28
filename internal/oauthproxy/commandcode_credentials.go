package oauthproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// commandcodeCredentialFile is the legacy credential filename ccl has always
// accepted under ~/.ccl/auth. New logins use a deterministic per-account name,
// but existing providers bound to this basename must keep working.
const commandcodeCredentialFile = "commandcode.json"

// commandcodeLoginTimeout bounds the /alpha/whoami validation call during an
// interactive login.
const commandcodeLoginTimeout = 15 * time.Second

// commandcodeOfficialAuthPath returns the official Command Code CLI's
// credential path (~/.commandcode/auth.json). The CLI stores a single
// long-lived user_... API key with no refresh mechanism, which makes an
// import-and-validate login the faithful integration.
func commandcodeOfficialAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".commandcode", "auth.json"), nil
}

// loginCommandCode imports the official CLI credential: it reads
// ~/.commandcode/auth.json, validates the key against /alpha/whoami, and
// persists the result to a deterministic per-account credential file. This is
// the non-browser counterpart of loginCommandCodeOAuth: users who signed in
// once with the official CLI can import its stored key instead of
// re-authenticating in a browser.
func loginCommandCode(ctx context.Context, authDir string) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	officialPath, err := commandcodeOfficialAuthPath()
	if err != nil {
		return LoginResult{}, err
	}
	raw, err := os.ReadFile(officialPath)
	if err != nil {
		return LoginResult{}, fmt.Errorf(
			"read official Command Code credential %q: %w "+
				"(install the official CLI and sign in once with your Command Code account)", officialPath, err)
	}
	var official struct {
		APIKey          string `json:"apiKey"`
		UserID          string `json:"userId"`
		UserName        string `json:"userName"`
		KeyName         string `json:"keyName"`
		AuthenticatedAt string `json:"authenticatedAt"`
	}
	if err := json.Unmarshal(raw, &official); err != nil {
		return LoginResult{}, fmt.Errorf("parse official Command Code credential: %w", err)
	}
	apiKey := strings.TrimSpace(official.APIKey)
	if apiKey == "" {
		return LoginResult{}, fmt.Errorf("official Command Code credential %s has no apiKey; sign in with the official CLI again", officialPath)
	}

	base := commandcodeAPIBase("")
	user, err := commandcodeValidateCredential(ctx, base, apiKey)
	if err != nil {
		return LoginResult{}, err
	}

	metadata := commandcodeCredentialMetadata(apiKey, user, map[string]any{
		"type":             ProviderCommandCode,
		"key_name":         strings.TrimSpace(official.KeyName),
		"authenticated_at": strings.TrimSpace(official.AuthenticatedAt),
		"source":           "official_cli_import",
	})
	return saveCommandCodeCredential(authDir, metadata)
}

// commandcodeCredentialMetadata creates one canonical persisted shape after the
// key has been validated. Identity fields are deliberately sourced from the
// gateway response, not from browser or official-CLI input.
func commandcodeCredentialMetadata(apiKey string, user *commandcodeWhoamiUser, extra map[string]any) map[string]any {
	metadata := make(map[string]any, len(extra)+5)
	maps.Copy(metadata, extra)
	metadata["type"] = ProviderCommandCode
	metadata["api_key"] = strings.TrimSpace(apiKey)
	if user != nil {
		if id := strings.TrimSpace(user.ID); id != "" {
			metadata["user_id"] = id
		}
		if username := strings.TrimSpace(user.UserName); username != "" {
			metadata["user_name"] = username
		} else if name := strings.TrimSpace(user.Name); name != "" {
			metadata["user_name"] = name
		}
		if name := strings.TrimSpace(user.Name); name != "" {
			metadata["name"] = name
		}
	}
	return metadata
}

// commandcodeCredentialFilename returns a stable, bounded filename for one
// validated account. The identity hash is derived from the authoritative
// whoami identity, never from the raw key. A key hash is used only when the
// server supplied no identity at all (which is not accepted by normal login,
// but keeps this helper safe for defensive callers).
func commandcodeCredentialFilename(metadata map[string]any) string {
	identity := firstMetadataString(metadata, "user_id", "user_name", "name")
	identity = strings.TrimSpace(identity)
	if identity != "" {
		canonical := strings.ToLower(identity)
		fragment := sanitizeCredentialIdentity(identity)
		if fragment == "" {
			fragment = "account"
		}
		if len(fragment) > 40 {
			fragment = fragment[:40]
		}
		sum := sha256.Sum256([]byte(canonical))
		return fmt.Sprintf("commandcode-%s-%s.json", fragment, hex.EncodeToString(sum[:4]))
	}

	key := firstMetadataString(metadata, "api_key", "apiKey", "key")
	sum := sha256.Sum256([]byte(key))
	return "commandcode-" + hex.EncodeToString(sum[:8]) + ".json"
}

// saveCommandCodeCredential atomically persists a Command Code credential to a
// per-account file under ~/.ccl/auth and returns the LoginResult. The legacy
// commandcode.json name remains loadable for existing configurations.
func saveCommandCodeCredential(authDir string, metadata map[string]any) (LoginResult, error) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Command Code credential: %w", err)
	}
	path := filepath.Join(authDir, commandcodeCredentialFilename(metadata))
	if err := writeCredentialAtomic(path, append(payload, '\n')); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderCommandCode, Backend: ProviderCommandCode, Path: path}, nil
}

// commandcodeWhoamiUser is the account identity carried by /alpha/whoami. The
// manual-paste login derives its stored metadata from this response, mirroring
// the official CLI's buildManualCommandAuthConfig.
type commandcodeWhoamiUser struct {
	ID       string `json:"id"`
	UserName string `json:"userName"`
	Name     string `json:"name"`
}

func commandcodeWhoamiUserFromJSON(raw []byte) (*commandcodeWhoamiUser, error) {
	var envelope struct {
		Valid bool                   `json:"valid"`
		User  *commandcodeWhoamiUser `json:"user"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode /alpha/whoami response: %w", err)
	}
	user := envelope.User
	if user == nil {
		user = &commandcodeWhoamiUser{}
	}
	// Gateways in the wild have returned both a nested user object and a flat
	// identity object. Accept the documented aliases, but never accept an
	// identity supplied by the browser callback or official CLI metadata.
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err == nil {
		if user.ID == "" {
			user.ID = commandcodeJSONFieldString(flat, "id", "userId", "user_id")
		}
		if user.UserName == "" {
			user.UserName = commandcodeJSONFieldString(flat, "userName", "username", "user_name")
		}
		if user.Name == "" {
			user.Name = commandcodeJSONFieldString(flat, "name", "displayName", "display_name")
		}
	}
	user.ID = strings.TrimSpace(user.ID)
	user.UserName = strings.TrimSpace(user.UserName)
	user.Name = strings.TrimSpace(user.Name)
	if user.ID == "" && user.UserName == "" && user.Name == "" {
		return nil, errors.New("/alpha/whoami returned no account identity")
	}
	return user, nil
}

func commandcodeJSONFieldString(fields map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// commandcodeValidateCredential calls /alpha/whoami and requires an
// authoritative account identity before a credential can be persisted.
func commandcodeValidateCredential(ctx context.Context, base, apiKey string) (*commandcodeWhoamiUser, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("Command Code credential has an empty API key")
	}
	return commandcodeWhoami(ctx, base, apiKey)
}

// commandcodeWhoami calls the lightweight /alpha/whoami route and returns the
// account identity for a valid key. 401/403 mean the key is invalid; anything
// else surfaces the raw status with a short body preview. A 2xx response with
// an empty or malformed body is not sufficient for credential creation.
func commandcodeWhoami(ctx context.Context, base, apiKey string) (*commandcodeWhoamiUser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/alpha/whoami", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-cli-environment", "production")
	request.Header.Set("x-command-code-version", commandcodeVersion)
	response, err := (&http.Client{Timeout: commandcodeLoginTimeout}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("validate Command Code key: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
		if readErr != nil {
			return nil, fmt.Errorf("read /alpha/whoami response: %w", readErr)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil, errors.New("/alpha/whoami returned an empty response")
		}
		return commandcodeWhoamiUserFromJSON(raw)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w by %s/alpha/whoami (HTTP %d); sign in with the official CLI again", errCommandCodeKeyRejected, base, response.StatusCode)
	default:
		return nil, fmt.Errorf("Command Code key validation failed (HTTP %d): %s", response.StatusCode, commandcodeErrorMessage(strings.TrimSpace(string(body)), response.StatusCode))
	}
}

// loadCommandCodeCredential reads an imported credential file under the auth
// dir and returns the upstream API key plus the full metadata for display.
func loadCommandCodeCredential(authDir, credentialFile string) (string, map[string]any, error) {
	credentialFile = strings.TrimSpace(credentialFile)
	if credentialFile == "" || credentialFile == "." {
		credentialFile = commandcodeCredentialFile
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read Command Code credential %s: %w", filepath.Base(path), err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", nil, fmt.Errorf("parse Command Code credential %s: %w", filepath.Base(path), err)
	}
	credentialType, _ := metadata["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(credentialType), ProviderCommandCode) {
		return "", nil, fmt.Errorf("credential %s is not a Command Code credential", filepath.Base(path))
	}
	apiKey := firstMetadataString(metadata, "api_key", "apiKey", "key")
	if strings.TrimSpace(apiKey) == "" {
		return "", nil, fmt.Errorf("Command Code credential %s has no API key", filepath.Base(path))
	}
	return apiKey, metadata, nil
}

// commandcodeListAuths builds the single-entry auth list for the imported
// credential. The official CLI stores one key, so the list never has more than
// one entry.
func commandcodeListAuths(path string) []*AuthInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil
	}
	return []*AuthInfo{{
		ID:       ProviderCommandCode,
		Provider: ProviderCommandCode,
		FileName: path,
		Label:    firstMetadataString(metadata, "user_name", "user_id", "key_name"),
		Status:   StatusActive,
		Metadata: metadata,
	}}
}

// startCommandCodeOAuth binds the imported Command Code credential to a
// loopback runtime. Unlike the other OAuth backends the key is long-lived:
// there is no refresh, so data-plane 401s are surfaced as-is. modelSpec is
// accepted for StartOAuth symmetry only; the runtime serves the authoritative
// static catalog and never rewrites requested model IDs.
func startCommandCodeOAuth(parent context.Context, _ string, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	apiKey, _, err := loadCommandCodeCredential(authDir, credentialFile)
	if err != nil {
		return nil, err
	}
	credentialPath := filepath.Join(authDir, filepath.Base(strings.TrimSpace(credentialFile)))
	if strings.TrimSpace(credentialFile) == "" || strings.TrimSpace(credentialFile) == "." {
		credentialPath = filepath.Join(authDir, commandcodeCredentialFile)
	}
	proxyRuntime, err := startCommandCodeRuntime(parent, "", apiKey)
	if err != nil {
		return nil, err
	}
	proxyRuntime.listAuths = func() []*AuthInfo { return commandcodeListAuths(credentialPath) }
	LogInfof("runtime start oauth provider=commandcode backend=commandcode protocol=commandcode local_endpoint=%q credential_file=%s models=%d auth_owner=ccl catalog_owner=ccl data_plane=ccl",
		SafeLogEndpoint(proxyRuntime.Endpoint()), filepath.Base(credentialFile), len(proxyRuntime.Models()))
	return proxyRuntime, nil
}
