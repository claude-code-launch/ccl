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
		case ProtectedEnvKey(key), key == "ANTHROPIC_MODEL", key == "ANTHROPIC_SMALL_FAST_MODEL",
			strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") && (strings.HasSuffix(key, "_MODEL") || strings.HasSuffix(key, "_MODEL_NAME")),
			strings.HasPrefix(key, "ANTHROPIC_CUSTOM_MODEL_OPTION"):
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+profile)
}
