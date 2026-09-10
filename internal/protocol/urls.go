package protocol

import (
	"net/url"
	"strings"
)

func normalizeEndpoint(endpoint, fallback string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fallback
	}
	return endpoint
}

// Rewrite only the escaped path. Query parameters, fragments and encoded path
// components must survive endpoint normalization unchanged.
func rewriteEndpointPath(endpoint string, rewrite func(string) string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return rewrite(endpoint)
	}
	escaped := rewrite(strings.TrimRight(u.EscapedPath(), "/"))
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return endpoint
	}
	u.Path, u.RawPath = path, escaped
	return u.String()
}

func normalizeOpenAIPath(baseURL, suffix string) string {
	return rewriteEndpointPath(normalizeEndpoint(baseURL, "https://api.openai.com/v1"), func(path string) string {
		for _, existing := range []string{"/chat/completions", "/responses", "/models"} {
			if strings.HasSuffix(path, existing) {
				path = strings.TrimSuffix(path, existing)
				break
			}
		}
		return path + suffix
	})
}

func NormalizeOpenAIModelsURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/models")
}

func NormalizeOpenAIChatCompletionsURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/chat/completions")
}

func NormalizeOpenAIResponsesURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/responses")
}

func normalizeAnthropicPath(baseURL, suffix string) string {
	return rewriteEndpointPath(normalizeEndpoint(baseURL, "https://api.anthropic.com/v1"), func(path string) string {
		for _, existing := range []string{"/messages", "/models"} {
			if strings.HasSuffix(path, existing) {
				return strings.TrimSuffix(path, existing) + suffix
			}
		}
		last := path[strings.LastIndex(path, "/")+1:]
		if path == "" || strings.EqualFold(last, "anthropic") || strings.EqualFold(last, "claude") {
			path += "/v1"
		}
		return path + suffix
	})
}

func NormalizeAnthropicModelsURL(baseURL string) string {
	return normalizeAnthropicPath(baseURL, "/models")
}

func NormalizeAnthropicMessagesURL(baseURL string) string {
	return normalizeAnthropicPath(baseURL, "/messages")
}

// NormalizeAnthropicBaseURLForClaude removes the suffix Claude appends itself.
func NormalizeAnthropicBaseURLForClaude(baseURL string) string {
	return rewriteEndpointPath(normalizeEndpoint(baseURL, "https://api.anthropic.com"), func(path string) string {
		for _, suffix := range []string{"/v1/messages", "/v1/models", "/v1"} {
			if strings.HasSuffix(path, suffix) {
				return strings.TrimSuffix(path, suffix)
			}
		}
		return path
	})
}
