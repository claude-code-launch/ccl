package oauthproxy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	ProviderCodex   = "codex"
	ProviderGemini  = "gemini"
	ProviderChatGPT = "gpt"
	// ProviderChatGPTLegacy is accepted by auth for older configs/docs.
	ProviderChatGPTLegacy = "chatgpt"
	ProviderGrok          = "grok"
	ProviderCopilot       = "copilot"
	ProviderQoder         = "qoder"
	ProviderKimi          = "kimi"
	ProviderKiro          = "kiro"
	ProviderWorkBuddy     = "workbuddy"
	ProviderCommandCode   = "commandcode"
	// backendXAI is the CLIProxyAPI authenticator provider key for xAI/Grok.
	backendXAI = "xai"
)

type LoginOptions struct {
	NoBrowser    bool
	CallbackPort int
	KiroAuthMode string
	// Stdin feeds manual credential entry (Command Code API-key paste); nil
	// disables the interactive paste path.
	Stdin io.Reader
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

	authDir, err := ensureAuthDir()
	if err != nil {
		return LoginResult{}, err
	}
	switch target {
	case ProviderKiro:
		return loginKiro(ctx, authDir, opts)
	case ProviderCopilot:
		return loginCopilot(ctx, authDir, opts)
	case ProviderQoder:
		return loginQoder(ctx, authDir, opts)
	case ProviderWorkBuddy:
		return loginWorkBuddy(ctx, authDir, opts)
	case ProviderChatGPT:
		return loginCodex(ctx, authDir, opts)
	case ProviderGemini:
		return loginGemini(ctx, authDir, opts)
	case ProviderGrok:
		return loginXai(ctx, authDir, opts)
	case ProviderKimi:
		return loginKimi(ctx, authDir, opts)
	case ProviderCommandCode:
		return loginCommandCodeOAuth(ctx, authDir, opts)
	default:
		return LoginResult{}, fmt.Errorf("unsupported OAuth provider %q", target)
	}
}

// ImportCredential is the non-browser import path for backends whose official
// CLI stores a long-lived key instead of offering a third-party OAuth flow.
// Browser/device OAuth stays on Login; imports stay here so `ccl oauth` never
// advertises a backend that cannot do OAuth.
func ImportCredential(ctx context.Context, providerName string) (LoginResult, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	switch target {
	case ProviderCommandCode:
		authDir, err := ensureAuthDir()
		if err != nil {
			return LoginResult{}, err
		}
		return loginCommandCode(ctx, authDir)
	default:
		return LoginResult{}, fmt.Errorf("unsupported import provider %q (use commandcode)", providerName)
	}
}

// ValidateLoginProvider returns the canonical public OAuth provider name.
// Codex remains an internal backend and a legacy runtime value, but new logins
// use the public GPT name (model family) because both routes authenticate the same account.
// Copilot is a separate GitHub OAuth and API backend. Command Code uses a
// browser "Get API key" page plus manual paste (its non-browser alternative is
// ImportCredential via `ccl import commandcode`).
func ValidateLoginProvider(providerName string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	switch target {
	case ProviderChatGPT, ProviderGemini, ProviderGrok, ProviderCopilot, ProviderQoder, ProviderKimi, ProviderKiro, ProviderWorkBuddy, ProviderCommandCode:
		return target, nil
	case ProviderChatGPTLegacy:
		// Keep accepting "chatgpt" as a login alias; canonicalize to "gpt".
		return ProviderChatGPT, nil
	default:
		return "", fmt.Errorf("unsupported auth provider %q (use gpt, gemini, grok, copilot, qoder, kimi, kiro, workbuddy, or commandcode)", providerName)
	}
}

func BackendProvider(providerName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case ProviderCodex, ProviderChatGPT, ProviderChatGPTLegacy:
		return ProviderCodex, nil
	case ProviderCopilot:
		return ProviderCopilot, nil
	case ProviderQoder:
		return ProviderQoder, nil
	case ProviderGemini:
		return "antigravity", nil
	case ProviderGrok, backendXAI:
		return backendXAI, nil
	case ProviderKimi:
		return ProviderKimi, nil
	case ProviderKiro:
		return ProviderKiro, nil
	case ProviderWorkBuddy:
		return ProviderWorkBuddy, nil
	case ProviderCommandCode:
		return ProviderCommandCode, nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider %q", providerName)
	}
}
