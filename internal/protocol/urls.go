package protocol

import "strings"

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
		return endpoint + "/models"
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
		return endpoint + "/messages"
	}
}
