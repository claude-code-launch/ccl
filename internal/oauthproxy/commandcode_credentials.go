package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// commandcodeCredentialFile is the single credential file ccl writes under
// ~/.ccl/auth. The official CLI keeps only one long-lived key in
// ~/.commandcode/auth.json, so one imported file is always authoritative and a
// re-login overwrites it in place. The browser OAuth flow (`ccl oauth
// commandcode`) writes the same file.
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
// persists the result to ~/.ccl/auth/commandcode.json. This is the
// non-browser counterpart of loginCommandCodeOAuth: users who signed in once
// with the official CLI can import its stored key instead of re-authenticating
// in a browser.
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
	if err := commandcodeValidateKey(ctx, base, apiKey); err != nil {
		return LoginResult{}, err
	}

	metadata := map[string]any{
		"type":             ProviderCommandCode,
		"api_key":          apiKey,
		"user_id":          strings.TrimSpace(official.UserID),
		"user_name":        strings.TrimSpace(official.UserName),
		"key_name":         strings.TrimSpace(official.KeyName),
		"authenticated_at": strings.TrimSpace(official.AuthenticatedAt),
		"source":           "official_cli_import",
	}
	return saveCommandCodeCredential(authDir, metadata)
}

// saveCommandCodeCredential atomically persists the Command Code credential to
// ~/.ccl/auth/commandcode.json and returns the LoginResult. Both the browser
// OAuth login and the official-CLI import funnel through here so the stored
// shape never diverges.
func saveCommandCodeCredential(authDir string, metadata map[string]any) (LoginResult, error) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Command Code credential: %w", err)
	}
	path := filepath.Join(authDir, commandcodeCredentialFile)
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

// commandcodeWhoami calls the lightweight /alpha/whoami route and returns the
// account identity for a valid key. 401/403 mean the key is invalid; anything
// else surfaces the raw status with a short body preview.
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
		var body struct {
			Valid bool                   `json:"valid"`
			User  *commandcodeWhoamiUser `json:"user"`
		}
		// The user payload is best-effort: a 2xx response still proves the key
		// is accepted even when the body is empty or unparseable.
		if err := json.NewDecoder(io.LimitReader(response.Body, chatMaxErrorBytes)).Decode(&body); err != nil {
			return nil, nil
		}
		return body.User, nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w by %s/alpha/whoami (HTTP %d); sign in with the official CLI again", errCommandCodeKeyRejected, base, response.StatusCode)
	default:
		return nil, fmt.Errorf("Command Code key validation failed (HTTP %d): %s", response.StatusCode, commandcodeErrorMessage(strings.TrimSpace(string(body)), response.StatusCode))
	}
}

// commandcodeValidateKey checks a user_... key against the lightweight
// /alpha/whoami route of the official gateway. Any 2xx response means the key
// is accepted; 401/403 mean the key is invalid, and everything else surfaces
// the raw status with a short body preview.
func commandcodeValidateKey(ctx context.Context, base, apiKey string) error {
	_, err := commandcodeWhoami(ctx, base, apiKey)
	return err
}

// loadCommandCodeCredential reads an imported credential file under the auth
// dir and returns the upstream API key plus the full metadata for display.
func loadCommandCodeCredential(authDir, credentialFile string) (string, map[string]any, error) {
	path := filepath.Join(authDir, filepath.Base(filepath.Clean(credentialFile)))
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
	credentialPath := filepath.Join(authDir, filepath.Base(filepath.Clean(credentialFile)))
	proxyRuntime, err := startCommandCodeRuntime(parent, "", apiKey)
	if err != nil {
		return nil, err
	}
	proxyRuntime.listAuths = func() []*AuthInfo { return commandcodeListAuths(credentialPath) }
	LogInfof("runtime start oauth provider=commandcode backend=commandcode protocol=commandcode local_endpoint=%q credential_file=%s models=%d auth_owner=ccl catalog_owner=ccl data_plane=ccl",
		SafeLogEndpoint(proxyRuntime.Endpoint()), filepath.Base(credentialFile), len(proxyRuntime.Models()))
	return proxyRuntime, nil
}
