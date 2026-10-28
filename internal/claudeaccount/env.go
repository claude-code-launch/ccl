package claudeaccount

import "strings"

// Environment keeps the user's normal environment while selecting an isolated
// Claude profile. Gateway credentials and platform selectors must not take
// precedence over that profile's Claude subscription.
func Environment(inherited []string, profile string) []string {
	env := make([]string, 0, len(inherited)+1)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		key = strings.ToUpper(key)
		switch {
		case key == "CLAUDE_CONFIG_DIR", key == "CLAUDE_SECURESTORAGE_CONFIG_DIR",
			key == "ANTHROPIC_API_KEY", key == "ANTHROPIC_AUTH_TOKEN", key == "ANTHROPIC_BASE_URL",
			key == "CLAUDE_CODE_HOST_CREDS_FILE", key == "CLAUDE_CODE_HOST_AUTH_ENV_VAR", key == "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
			key == "ANTHROPIC_CUSTOM_HEADERS", key == "ANTHROPIC_MODEL", key == "ANTHROPIC_SMALL_FAST_MODEL",
			strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") && (strings.HasSuffix(key, "_MODEL") || strings.HasSuffix(key, "_MODEL_NAME")),
			strings.HasPrefix(key, "ANTHROPIC_CUSTOM_MODEL_OPTION"),
			strings.HasPrefix(key, "CLAUDE_CODE_OAUTH_TOKEN"), key == "CLAUDE_CODE_USE_BEDROCK",
			key == "CLAUDE_CODE_USE_VERTEX", key == "CLAUDE_CODE_USE_FOUNDRY",
			key == "CLAUDE_CODE_USE_MANTLE", key == "CLAUDE_CODE_USE_GATEWAY":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+profile)
}
