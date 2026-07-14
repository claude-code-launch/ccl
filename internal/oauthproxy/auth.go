package oauthproxy

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	ProviderCodex   = "codex"
	ProviderGemini  = "gemini"
	ProviderChatGPT = "chatgpt"
)

type LoginOptions struct {
	NoBrowser    bool
	CallbackPort int
}

type LoginResult struct {
	Provider string
	Backend  string
	Path     string
}

func AuthDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ccl", "auth"), nil
}

func ensureAuthDir() (string, error) {
	authDir, err := AuthDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", fmt.Errorf("create auth directory: %w", err)
	}
	if err := os.Chmod(authDir, 0o700); err != nil {
		return "", fmt.Errorf("secure auth directory: %w", err)
	}
	return authDir, nil
}

func Login(ctx context.Context, providerName string, opts LoginOptions) (LoginResult, error) {
	target, err := ValidateLoginProvider(providerName)
	if err != nil {
		return LoginResult{}, err
	}
	backend, err := BackendProvider(target)
	if err != nil {
		return LoginResult{}, err
	}
	_, authenticator, err := authenticatorFor(target)
	if err != nil {
		return LoginResult{}, err
	}

	authDir, err := ensureAuthDir()
	if err != nil {
		return LoginResult{}, err
	}

	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := sdkauth.NewManager(store, authenticator)
	cfg := &sdkconfig.Config{AuthDir: authDir}

	record, path, err := manager.Login(ctx, backend, cfg, &sdkauth.LoginOptions{
		NoBrowser:    opts.NoBrowser,
		CallbackPort: opts.CallbackPort,
		Prompt:       promptLine,
	})
	if err != nil {
		return LoginResult{}, err
	}
	if record == nil || strings.TrimSpace(path) == "" {
		return LoginResult{}, fmt.Errorf("%s authentication did not persist credentials", target)
	}

	return LoginResult{Provider: target, Backend: backend, Path: path}, nil
}

// ValidateLoginProvider returns the canonical public OAuth provider name.
// Codex remains an internal backend and a legacy runtime value, but new logins
// use the ChatGPT name because both routes authenticate the same account.
func ValidateLoginProvider(providerName string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	switch target {
	case ProviderChatGPT, ProviderGemini:
		return target, nil
	default:
		return "", fmt.Errorf("unsupported auth provider %q (use chatgpt or gemini)", providerName)
	}
}

func BackendProvider(providerName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case ProviderCodex, ProviderChatGPT:
		return ProviderCodex, nil
	case ProviderGemini:
		return "antigravity", nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider %q", providerName)
	}
}

func authenticatorFor(providerName string) (string, sdkauth.Authenticator, error) {
	backend, err := BackendProvider(providerName)
	if err != nil {
		return "", nil, err
	}
	switch backend {
	case ProviderCodex:
		return ProviderCodex, sdkauth.NewCodexAuthenticator(), nil
	case "antigravity":
		// CLIProxyAPI exposes Gemini subscription models through its Google
		// Antigravity OAuth backend.
		return "antigravity", sdkauth.NewAntigravityAuthenticator(), nil
	}
	return "", nil, fmt.Errorf("unsupported auth backend %q", backend)
}

func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
