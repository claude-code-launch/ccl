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
	ProviderChatGPT = "gpt"
	// ProviderChatGPTLegacy is accepted by auth for older configs/docs.
	ProviderChatGPTLegacy = "chatgpt"
	ProviderGrok    = "grok"
	ProviderCopilot = "copilot"
	ProviderKimi    = "kimi"
	ProviderClaude  = "claude"
	// backendXAI is the CLIProxyAPI authenticator provider key for xAI/Grok.
	backendXAI = "xai"
	// codexLoginModeMetadata mirrors CLIProxyAPI's sdk/auth LoginOptions key.
	codexLoginModeMetadataKey = "codex_login_mode"
	codexLoginModeDevice      = "device"
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

	sdkOpts := &sdkauth.LoginOptions{
		NoBrowser:    opts.NoBrowser,
		CallbackPort: opts.CallbackPort,
		Prompt:       promptLine,
	}
	// GitHub Copilot logins through ChatGPT share the Codex backend but use the
	// OpenAI device-code flow (GitHub-backed) instead of the OAuth redirect flow.
	if target == ProviderCopilot {
		if sdkOpts.Metadata == nil {
			sdkOpts.Metadata = map[string]string{}
		}
		sdkOpts.Metadata[codexLoginModeMetadataKey] = codexLoginModeDevice
		sdkOpts.CallbackPort = 0 // device flow has no local callback
	}

	record, path, err := manager.Login(ctx, backend, cfg, sdkOpts)
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
// use the public GPT name (model family) because both routes authenticate the same account.
// Copilot reuses the Codex backend but logs in through the GitHub-backed device
// flow, so it is exposed as its own public provider.
func ValidateLoginProvider(providerName string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	switch target {
	case ProviderChatGPT, ProviderGemini, ProviderGrok, ProviderCopilot, ProviderKimi, ProviderClaude:
		return target, nil
	case ProviderChatGPTLegacy:
		// Keep accepting "chatgpt" as a login alias; canonicalize to "gpt".
		return ProviderChatGPT, nil
	default:
		return "", fmt.Errorf("unsupported auth provider %q (use gpt, gemini, grok, copilot, kimi, or claude)", providerName)
	}
}

func BackendProvider(providerName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case ProviderCodex, ProviderChatGPT, ProviderChatGPTLegacy, ProviderCopilot:
		return ProviderCodex, nil
	case ProviderGemini:
		return "antigravity", nil
	case ProviderGrok, backendXAI:
		return backendXAI, nil
	case ProviderKimi:
		return ProviderKimi, nil
	case ProviderClaude:
		return ProviderClaude, nil
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
	case backendXAI:
		// CLIProxyAPI exposes xAI/Grok subscription models through its xAI
		// OAuth device-code backend.
		return backendXAI, sdkauth.NewXAIAuthenticator(), nil
	case ProviderKimi:
		return ProviderKimi, sdkauth.NewKimiAuthenticator(), nil
	case ProviderClaude:
		return ProviderClaude, sdkauth.NewClaudeAuthenticator(), nil
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
