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
	if endpointPathEmpty(endpoint) {
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
