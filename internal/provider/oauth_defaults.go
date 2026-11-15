package provider

import "strings"

// retiredGeminiHaikuModel is the Gemini default CCL used to write.
// Neither Antigravity catalog still lists it, and requesting it returns 404.
const (
	retiredGeminiHaikuModel = "gemini-3.1-pro-low"
	geminiFlashReplacement  = "gemini-3.8-flash"
)

// OAuthDefaultModels returns the fallback model pool for OAuth backends whose
// startup path requires static model IDs before live /v1/models discovery.
// Backends with eager live /models discovery return "".
func OAuthDefaultModels(oauthProvider string) string {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "kimi":
		// Kimi Code exposes no public model-list endpoint, so the runtime needs
		// its canonical upstream model IDs to start.
		return "kimi-for-coding,kimi-for-coding-highspeed"
	case "kiro":
		// Kiro discovers /v1/models lazily on HTTP GET, so Runtime.Models() at
		// startup reads the configured pool.
		return "claude-opus-4-6,claude-sonnet-4-6,claude-haiku-4-5"
	default:
		return ""
	}
}

// stripContextSuffix removes display-only context markers such as [1m] so
// model IDs match catalog entries that omit the suffix.
func stripContextSuffix(model string) string {
	base := strings.TrimSpace(model)
	for len(base) >= 4 && strings.EqualFold(base[len(base)-4:], "[1m]") {
		base = strings.TrimSpace(base[:len(base)-4])
	}
	return base
}

// StripContextSuffix removes the [1m] context marker(s) from a model ID.
func StripContextSuffix(model string) string { return stripContextSuffix(model) }

// HasOneMSuffix reports whether model ends with a [1m] (or [1M]) context marker.
func HasOneMSuffix(model string) bool {
	trimmed := strings.TrimSpace(model)
	return len(trimmed) >= 4 && strings.EqualFold(trimmed[len(trimmed)-4:], "[1m]")
}

// NormalizeModelID normalizes a single configured default model ID, keeping at
// most one lowercase [1m] marker when present.
func NormalizeModelID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, ",") {
		parts := strings.Split(trimmed, ",")
		trimmed = ""
		for _, part := range parts {
			if candidate := strings.TrimSpace(part); candidate != "" {
				trimmed = candidate
				break
			}
		}
		if trimmed == "" {
			return ""
		}
	}
	has1M := HasOneMSuffix(trimmed)
	bare := stripContextSuffix(trimmed)
	if bare == "" {
		return ""
	}
	if has1M {
		return bare + "[1m]"
	}
	return bare
}
