package claudeaccount

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// ProtectedEnvKey identifies settings that can replace a subscription's
// identity or send its bearer token to a different service.
func ProtectedEnvKey(key string) bool {
	key = strings.ToUpper(key)
	switch key {
	case "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_HOST_CREDS_FILE",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY":
		return true
	}
	return strings.HasPrefix(key, "CLAUDE_CODE_OAUTH_TOKEN")
}

// Native accounts use their own Claude configuration. Reject CCL proxy/model
// options instead of accepting settings that this launch deliberately ignores.
func ValidateProvider(p provider.Provider) error {
	if (p.Type != "" && !provider.IsAnthropicType(p.Type)) || (p.Endpoint != "" && p.Endpoint != "oauth://claude") ||
		len(p.Env) > 0 || p.APIKey != "" || p.AnthropicAuth != "" || p.Model != "" ||
		p.CustomModelID != "" || p.OpusModel != "" || p.SonnetModel != "" || p.HaikuModel != "" ||
		p.FableModel != "" || p.SubagentModel != "" || len(p.ModelOverrides) > 0 ||
		p.EffortLevel != "" || p.Ultracode || p.FastMode || p.StatuslineDisabled || p.ContextPreset != "" ||
		len(p.ModelProtocols) > 0 || p.ModelsDevProvider != "" {
		return OptionsError(p.Name)
	}
	return nil
}

func OptionsError(name string) error {
	return fmt.Errorf("Claude account %q uses its native profile; clear CCL provider env/model/effort options and use Claude Code's /config, /model or /effort instead", name)
}

// RoutingEnv is also written at the final settings layer. Removing inherited
// variables alone is insufficient: project settings can introduce them again.
func RoutingEnv(profile string) map[string]string {
	env := map[string]string{
		"CLAUDE_CONFIG_DIR": profile, "CLAUDE_SECURESTORAGE_CONFIG_DIR": profile,
		"ANTHROPIC_BASE_URL": "https://api.anthropic.com",
		"ANTHROPIC_API_KEY":  "", "ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR": "", "CLAUDE_CODE_HOST_CREDS_FILE": "",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR": "", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "",
	}
	for _, platform := range []string{"BEDROCK", "VERTEX", "FOUNDRY", "MANTLE", "GATEWAY"} {
		env["CLAUDE_CODE_USE_"+platform] = "0"
	}
	return env
}

func conflictingEnv(key, value string) bool {
	if !ProtectedEnvKey(key) || strings.TrimSpace(value) == "" {
		return false
	}
	key = strings.ToUpper(key)
	if strings.HasPrefix(key, "CLAUDE_CODE_USE_") || key == "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST" {
		return value != "0" && !strings.EqualFold(value, "false")
	}
	if key == "ANTHROPIC_BASE_URL" {
		u, err := url.Parse(value)
		return err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "api.anthropic.com") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != ""
	}
	return true
}

// CheckSettings refuses conflicting authentication instead of silently
// replacing an account or a gateway the user configured. Errors name the
// source and key, never its value.
func CheckSettings(profile, cwd string) error {
	paths := []string{filepath.Join(profile, "settings.json"), filepath.Join(profile, "managed-settings.json")}
	for dir := cwd; dir != ""; {
		paths = append(paths, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for _, key := range []string{"CLAUDE_CODE_MANAGED_SETTINGS_PATH", "CLAUDE_CODE_REMOTE_SETTINGS_PATH"} {
		if path := os.Getenv(key); path != "" {
			paths = append(paths, path)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths, "/Library/Application Support/ClaudeCode/managed-settings.json")
	case "windows":
		base := os.Getenv("ProgramFiles")
		if base == "" {
			base = `C:\Program Files`
		}
		paths = append(paths, filepath.Join(base, "ClaudeCode", "managed-settings.json"))
	default:
		paths = append(paths, "/etc/claude-code/managed-settings.json")
	}
	for _, path := range paths {
		if err := checkSettingsFile(path); err != nil {
			return err
		}
	}
	return nil
}

func checkSettingsFile(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check Claude account settings %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return fmt.Errorf("cannot safely read Claude account settings %q", path)
	}
	return checkSettingsJSON(data, path)
}

func checkSettingsJSON(data []byte, source string) error {
	var fields struct {
		APIKeyHelper     string            `json:"apiKeyHelper"`
		ForceLoginMethod string            `json:"forceLoginMethod"`
		Env              map[string]string `json:"env"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return fmt.Errorf("invalid Claude account settings in %q", source)
	}
	if strings.TrimSpace(fields.APIKeyHelper) != "" {
		return fmt.Errorf("Claude subscription account conflicts with apiKeyHelper in %q; remove the conflicting setting or select an API provider", source)
	}
	if fields.ForceLoginMethod != "" && fields.ForceLoginMethod != "claudeai" {
		return fmt.Errorf("Claude subscription account conflicts with forceLoginMethod in %q; use claudeai or select an API provider", source)
	}
	for key, value := range fields.Env {
		if conflictingEnv(key, value) {
			return fmt.Errorf("Claude subscription account conflicts with %s in %q; remove the conflicting setting or select an API provider", key, source)
		}
	}
	return nil
}

// CheckArgs inspects extra --settings inputs without changing the arguments
// sent to Claude. Preferences remain native; conflicting auth is refused.
func CheckArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		var value string
		switch {
		case arg == "--settings":
			i++
			if i == len(args) {
				return fmt.Errorf("--settings requires a file or JSON object")
			}
			value = args[i]
		case strings.HasPrefix(arg, "--settings="):
			value = strings.TrimPrefix(arg, "--settings=")
		default:
			// A required value may itself be "--" or "--settings". It is
			// not an option boundary; Commander consumes it for that flag.
			if claudeOptionRequiresValue(arg) && i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(value), "{") {
			if err := checkSettingsJSON([]byte(value), "--settings"); err != nil {
				return err
			}
		} else {
			if _, err := os.Stat(value); err != nil {
				return fmt.Errorf("cannot check --settings file %q", value)
			}
			if err := checkSettingsFile(value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Required-value options of the supported Claude CLI. Optional-value flags
// (such as --resume and --debug) deliberately leave -- as an option boundary.
func claudeOptionRequiresValue(arg string) bool {
	switch arg {
	case "--add-dir", "--agent", "--agents", "--allowedTools", "--allowed-tools",
		"--append-system-prompt", "--append-system-prompt-file", "--autocompact", "--betas",
		"--debug-file", "--disallowedTools", "--disallowed-tools", "--effort", "--environment",
		"--fallback-model", "--file", "--input-format", "--json-schema", "--max-budget-usd",
		"--max-turns", "--max-thinking-tokens", "--mcp-config", "--model", "-n", "--name",
		"--output-format", "--permission-mode", "--permission-prompt-tool", "--permission-prompts",
		"--plugin-dir", "--plugin-url", "--remote-control-session-name-prefix", "--session-id",
		"--setting-sources", "--system-prompt", "--system-prompt-file", "--system-prompt-snapshot", "--tools":
		return true
	}
	return false
}
