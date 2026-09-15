package oauthproxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1" // #nosec G505 -- Chromium safeStorage compatibility requires SHA-1 PBKDF2.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

const (
	autoclawDesktopAuthFile = "auth.json"
	autoclawEncryptedPrefix = "enc:"
	autoclawChromiumPrefix  = "v10"
	autoclawDefaultVersion  = "1.18.5"

	// Electron's Chromium OSCrypt parameters. AutoClaw 1.18.5 stores the
	// auth.json values as enc:base64(v10 + AES-128-CBC(ciphertext)).
	autoclawSafeStorageService = "Chromium Safe Storage"
	autoclawSafeStorageSalt    = "saltysalt"
	autoclawSafeStorageRounds  = 1003
)

var (
	// Tests replace this resolver; production reads AutoClaw's own login state.
	autoclawDesktopAuthPath = defaultAutoClawDesktopAuthPath
	// Keep Keychain access behind a function so decryption is independently
	// testable without depending on a developer's login keychain.
	autoclawSafeStoragePassword = readAutoClawSafeStoragePassword
)

type autoclawDesktopAuth struct {
	DeviceID     string                  `json:"deviceId"`
	UpdatedAt    int64                   `json:"updatedAt"`
	Token        string                  `json:"token"`
	RefreshToken string                  `json:"refreshToken"`
	UserInfo     autoclawDesktopUserInfo `json:"userInfo"`
}

type autoclawDesktopUserInfo struct {
	ID       json.Number `json:"id"`
	UserID   string      `json:"user_id"`
	UserName string      `json:"user_name"`
	Email    string      `json:"email"`
}

// AutoClawDesktopAuthPath reports where AutoClaw's desktop login state lives on
// this platform. Callers that need to place or inspect that file — a test
// seeding a fixture, most of all — must ask here rather than re-deriving the
// per-platform rules: an XDG_CONFIG_HOME or APPDATA in the environment moves the
// real path, and a second copy of these rules silently drifts out of step.
func AutoClawDesktopAuthPath() (string, error) {
	return autoclawDesktopAuthPath()
}

func defaultAutoClawDesktopAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "autoclaw", autoclawDesktopAuthFile), nil
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "autoclaw", autoclawDesktopAuthFile), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "autoclaw", autoclawDesktopAuthFile), nil
	default:
		if configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configHome != "" {
			return filepath.Join(configHome, "autoclaw", autoclawDesktopAuthFile), nil
		}
		return filepath.Join(home, ".config", "autoclaw", autoclawDesktopAuthFile), nil
	}
}

func readAutoClawSafeStoragePassword(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("AutoClaw safeStorage import is currently supported on macOS; sign in on macOS and run `ccl oauth autoclaw`")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(lookupCtx, "/usr/bin/security", "find-generic-password", "-w", "-s", autoclawSafeStorageService).Output()
	if err != nil {
		return "", fmt.Errorf("read %s from macOS Keychain: %w", autoclawSafeStorageService, err)
	}
	password := strings.TrimSpace(string(out))
	if password == "" {
		return "", fmt.Errorf("macOS Keychain item %q is empty", autoclawSafeStorageService)
	}
	return password, nil
}

// loadAutoClawDesktopAuth reads and decrypts AutoClaw's local OAuth state. It
// never writes the desktop app's files; CCL copies usable tokens to its own
// 0600 credential and no longer needs the AutoClaw process at runtime.
func loadAutoClawDesktopAuth(ctx context.Context) (autoclawDesktopAuth, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := autoclawDesktopAuthPath()
	if err != nil {
		return autoclawDesktopAuth{}, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return autoclawDesktopAuth{}, path, fmt.Errorf("AutoClaw login state was not found at %s; sign in to AutoClaw once, then rerun `ccl oauth autoclaw`", path)
		}
		return autoclawDesktopAuth{}, path, fmt.Errorf("read AutoClaw login state %s: %w", path, err)
	}
	var state autoclawDesktopAuth
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		return autoclawDesktopAuth{}, path, fmt.Errorf("decode AutoClaw login state %s: %w", path, err)
	}

	password := ""
	if strings.HasPrefix(strings.TrimSpace(state.Token), autoclawEncryptedPrefix) ||
		strings.HasPrefix(strings.TrimSpace(state.RefreshToken), autoclawEncryptedPrefix) {
		password, err = autoclawSafeStoragePassword(ctx)
		if err != nil {
			return autoclawDesktopAuth{}, path, err
		}
	}
	state.Token, err = decryptAutoClawStoredValue(state.Token, password)
	if err != nil {
		return autoclawDesktopAuth{}, path, fmt.Errorf("decrypt AutoClaw access token: %w", err)
	}
	state.RefreshToken, err = decryptAutoClawStoredValue(state.RefreshToken, password)
	if err != nil {
		return autoclawDesktopAuth{}, path, fmt.Errorf("decrypt AutoClaw refresh token: %w", err)
	}
	state.Token = stripBearerPrefix(state.Token)
	state.RefreshToken = strings.TrimSpace(state.RefreshToken)
	state.DeviceID = strings.TrimSpace(state.DeviceID)
	if state.Token == "" || state.RefreshToken == "" || state.DeviceID == "" {
		return autoclawDesktopAuth{}, path, errors.New("AutoClaw login state is incomplete (access token, refresh token, or device ID is missing); sign in to AutoClaw again")
	}
	return state, path, nil
}

func decryptAutoClawStoredValue(value, password string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, autoclawEncryptedPrefix) {
		return value, nil
	}
	encoded := strings.TrimPrefix(value, autoclawEncryptedPrefix)
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode encrypted value: %w", err)
	}
	if len(ciphertext) < len(autoclawChromiumPrefix) || string(ciphertext[:len(autoclawChromiumPrefix)]) != autoclawChromiumPrefix {
		return "", errors.New("encrypted value does not use Chromium v10 format")
	}
	ciphertext = ciphertext[len(autoclawChromiumPrefix):]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("encrypted value has an invalid AES-CBC length")
	}
	key := pbkdf2.Key([]byte(password), []byte(autoclawSafeStorageSalt), autoclawSafeStorageRounds, 16, sha1.New) // #nosec G505
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plaintext := make([]byte, len(ciphertext))
	iv := []byte("                ")
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	plaintext, err = unpadAutoClawPKCS7(plaintext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func unpadAutoClawPKCS7(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("decrypted value is empty")
	}
	padding := int(value[len(value)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(value) {
		return nil, errors.New("decrypted value has invalid padding")
	}
	for _, b := range value[len(value)-padding:] {
		if int(b) != padding {
			return nil, errors.New("decrypted value has invalid padding")
		}
	}
	return value[:len(value)-padding], nil
}

func stripBearerPrefix(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(value[len("Bearer "):])
	}
	return value
}

// AutoClawOpenAIBaseURL is the OpenAI Chat Completions base used by the
// AutoClaw desktop app's managed zai provider. CCL appends /chat/completions.
func AutoClawOpenAIBaseURL() string {
	return strings.TrimRight(autoclawAPIOrigin, "/") + "/autoclaw-proxy/proxy/autoclaw"
}

// AutoClawAnthropicBaseURL remains for config migration and old callers. The
// AutoClaw runtime is now OpenAI Chat based; new providers use OpenAIBaseURL.
func AutoClawAnthropicBaseURL() string { return AutoClawOpenAIBaseURL() }

// loginAutoClaw imports AutoClaw's completed OAuth session. Google OAuth's
// redirect terminates at the desktop app's loopback server, so AutoClaw owns
// the initial sign-in; after this one-time import CCL refreshes independently.
func loginAutoClaw(ctx context.Context, authDir string, _ LoginOptions) (LoginResult, error) {
	state, sourcePath, err := loadAutoClawDesktopAuth(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	metadata := autoClawMetadataFromDesktop(state, sourcePath)
	result, err := saveAutoClawCredential(authDir, metadata)
	if err != nil {
		return LoginResult{}, err
	}
	fmt.Println("AutoClaw authentication imported; CCL will refresh it independently")
	return result, nil
}

func autoClawMetadataFromDesktop(state autoclawDesktopAuth, sourcePath string) map[string]any {
	metadata := map[string]any{
		"type":             ProviderAutoClaw,
		"access_token":     stripBearerPrefix(state.Token),
		"refresh_token":    strings.TrimSpace(state.RefreshToken),
		"device_id":        strings.TrimSpace(state.DeviceID),
		"base_url":         AutoClawOpenAIBaseURL(),
		"app_version":      autoClawInstalledVersion(),
		"authenticated_at": time.Now().UTC().Format(time.RFC3339),
		"source":           "autoclaw_desktop",
		"source_file":      filepath.Base(sourcePath),
	}
	if state.UpdatedAt > 0 {
		metadata["desktop_updated_at"] = state.UpdatedAt
	}
	if id := strings.TrimSpace(state.UserInfo.UserID); id != "" {
		metadata["user_id"] = id
	} else if id := strings.TrimSpace(state.UserInfo.ID.String()); id != "" {
		metadata["user_id"] = id
	}
	if email := strings.TrimSpace(state.UserInfo.Email); email != "" {
		metadata["email"] = email
	}
	if name := strings.TrimSpace(state.UserInfo.UserName); name != "" {
		metadata["name"] = name
	}
	if expiresAt := autoclawJWTExpiry(state.Token); !expiresAt.IsZero() {
		metadata["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	return metadata
}

func autoClawInstalledVersion() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :CFBundleShortVersionString", "/Applications/AutoClaw.app/Contents/Info.plist").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
	}
	return autoclawDefaultVersion
}

func autoclawJWTExpiry(token string) time.Time {
	parts := strings.Split(stripBearerPrefix(token), ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}
	}
	var seconds int64
	if value := claims["exp"]; len(value) > 0 {
		var number json.Number
		if json.Unmarshal(value, &number) == nil {
			seconds, _ = number.Int64()
		}
	}
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func autoclawCredentialFilename(metadata map[string]any) string {
	identity := strings.TrimSpace(firstMetadataString(metadata, "user_id", "email", "name"))
	if identity != "" {
		fragment := sanitizeCredentialIdentity(identity)
		if fragment == "" {
			fragment = "account"
		}
		if len(fragment) > 40 {
			fragment = fragment[:40]
		}
		return ProviderAutoClaw + "-" + fragment + "-" + autoclawIdentityHash(strings.ToLower(identity)) + ".json"
	}
	return ProviderAutoClaw + "-" + autoclawIdentityHash(firstMetadataString(metadata, "access_token")) + ".json"
}

func autoclawIdentityHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:4])
}

func saveAutoClawCredential(authDir string, metadata map[string]any) (LoginResult, error) {
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode AutoClaw credential: %w", err)
	}
	path := filepath.Join(authDir, autoclawCredentialFilename(metadata))
	if err := writeCredentialAtomic(path, append(payload, '\n')); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderAutoClaw, Backend: ProviderAutoClaw, Path: path}, nil
}
