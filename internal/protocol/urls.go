package protocol

import (
	"net/url"
	"strings"
)

func normalizeEndpoint(endpoint, fallback string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return fallback
	}
	return endpoint
}

func NormalizeOpenAIModelsURL(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.openai.com/v1")

	switch {
	case strings.HasSuffix(endpoint, "/models"):
		return endpoint
	case strings.HasSuffix(endpoint, "/chat/completions"):
		return strings.TrimSuffix(endpoint, "/chat/completions") + "/models"
	default:
		return endpoint + "/models"
	}
}

func NormalizeOpenAIChatCompletionsURL(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.openai.com/v1")

	switch {
	case strings.HasSuffix(endpoint, "/chat/completions"):
		return endpoint
	case strings.HasSuffix(endpoint, "/models"):
		return strings.TrimSuffix(endpoint, "/models") + "/chat/completions"
	default:
		return endpoint + "/chat/completions"
	}
}

func NormalizeOpenAIResponsesURL(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.openai.com/v1")

	switch {
	case strings.HasSuffix(endpoint, "/responses"):
		return endpoint
	case strings.HasSuffix(endpoint, "/chat/completions"):
		return strings.TrimSuffix(endpoint, "/chat/completions") + "/responses"
	case strings.HasSuffix(endpoint, "/models"):
		return strings.TrimSuffix(endpoint, "/models") + "/responses"
	default:
		return endpoint + "/responses"
	}
}

// IsCodexBaseEndpoint reports whether baseURL points at a dedicated Codex base
// path such as https://example.com/codex. Model discovery is performed at the
// corresponding /codex/models endpoint without rewriting the configured base.
func IsCodexBaseEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return false
	}
	parts := splitEndpointPath(u.Path)
	return len(parts) > 0 && strings.EqualFold(parts[len(parts)-1], "codex")
}

// InvalidCodexV1EndpointSuggestion identifies a user-supplied Codex generation
// endpoint ending in /codex/v1 and returns the base endpoint the user should
// enter instead. ccl reports this suggestion but never rewrites the setting.
func InvalidCodexV1EndpointSuggestion(endpoint string) (string, bool) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	parts := splitEndpointPath(u.Path)
	if len(parts) > 0 {
		switch strings.ToLower(parts[len(parts)-1]) {
		case "models", "responses":
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) < 2 || !strings.EqualFold(parts[len(parts)-2], "codex") || !strings.EqualFold(parts[len(parts)-1], "v1") {
		return "", false
	}
	u.Path = "/" + strings.Join(parts[:len(parts)-1], "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), true
}

func splitEndpointPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func NormalizeAnthropicModelsURL(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.anthropic.com/v1")

	switch {
	case strings.HasSuffix(endpoint, "/models"):
		return endpoint
	case strings.HasSuffix(endpoint, "/messages"):
		return strings.TrimSuffix(endpoint, "/messages") + "/models"
	default:
		return appendAnthropicPath(endpoint, "/models")
	}
}

func NormalizeAnthropicMessagesURL(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.anthropic.com/v1")

	switch {
	case strings.HasSuffix(endpoint, "/messages"):
		return endpoint
	case strings.HasSuffix(endpoint, "/models"):
		return strings.TrimSuffix(endpoint, "/models") + "/messages"
	default:
		return appendAnthropicPath(endpoint, "/messages")
	}
}

// NormalizeAnthropicBaseURLForClaude returns the base URL shape expected by
// Claude Code's Anthropic client. Claude appends /v1/messages itself, so a
// configured endpoint ending in /v1, /v1/messages, or /v1/models would otherwise
// become /v1/v1/messages at runtime.
func NormalizeAnthropicBaseURLForClaude(baseURL string) string {
	endpoint := normalizeEndpoint(baseURL, "https://api.anthropic.com")
	for _, suffix := range []string{"/v1/messages", "/v1/models", "/v1"} {
		if strings.HasSuffix(endpoint, suffix) {
			return strings.TrimSuffix(endpoint, suffix)
		}
	}
	return endpoint
}

func appendAnthropicPath(endpoint, suffix string) string {
	if endpointPathEmpty(endpoint) || endpointPathHasAnySuffix(endpoint, "anthropic", "claude") {
		return endpoint + "/v1" + suffix
	}
	return endpoint + suffix
}

func endpointPathEmpty(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.Trim(u.Path, "/") == ""
}

func endpointPathHasAnySuffix(endpoint string, suffixes ...string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return false
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	for _, suffix := range suffixes {
		if strings.EqualFold(last, suffix) {
			return true
		}
	}
	return false
}
