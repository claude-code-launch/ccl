package claude

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// AgentLaunch owns only CCL's provider runtime. The ACP adapter owns the SDK
// and its Claude executable, so preparing this launch needs no installed CLI
// and writes no temporary Claude settings file.
type AgentLaunch struct {
	prepared *providerContext
	cancel   context.CancelFunc
}

func PrepareAgentContext(ctx context.Context, p provider.Provider) (*AgentLaunch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Preparation follows the caller's cancellation, but a successful runtime
	// is owned by the lease until Close, not by the short configuration request.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	prepared, err := setupProvider(lifetime, p)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if !stop() || ctx.Err() != nil {
		prepared.cleanup()
		cancel()
		return nil, ctx.Err()
	}
	return &AgentLaunch{prepared: prepared, cancel: cancel}, nil
}

func (l *AgentLaunch) Close() {
	if l != nil && l.prepared != nil {
		l.cancel()
		l.prepared.cleanup()
	}
}

// Options is the upstream adapter's documented _meta.claudeCode.options
// object. Resolve preferences for the client's cwd, not CCL's startup cwd.
func (l *AgentLaunch) Options(cwd string) (json.RawMessage, error) {
	copy := *l.prepared
	settings := copy.settingsForCWD(cwd)
	settings.Env = completeAgentEnv(settings, copy.useProxy)
	fields, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(fields, &object); err != nil {
		return nil, err
	}
	// Old user mappings and key helpers must not redirect a CCL provider.
	if len(object["modelOverrides"]) == 0 {
		object["modelOverrides"] = json.RawMessage(`{}`)
	}
	object["apiKeyHelper"] = json.RawMessage(`""`)
	return json.Marshal(struct {
		Env      map[string]string          `json:"env"`
		Settings map[string]json.RawMessage `json:"settings"`
	}{settings.Env, object})
}

// Complete every routing/model key because the adapter's process environment
// outlives any one provider generation. Empty values neutralize inherited
// settings while a later provider's real values replace them.
func completeAgentEnv(settings settingsJSON, useProxy bool) map[string]string {
	env := settings.Env
	for _, key := range append(inheritedEnvToDrop(settings, useProxy),
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
		SubagentModelEnv, "CLAUDE_CODE_ATTRIBUTION_HEADER", "CLAUDE_CODE_GATEWAY_HINT_HEADERS") {
		if _, ok := env[key]; !ok {
			value := ""
			if strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
				value = "0"
			}
			if key == "ANTHROPIC_CUSTOM_HEADERS" && !useProxy {
				value = os.Getenv(key)
			}
			env[key] = value
		}
	}
	for _, key := range provider.ManagedContextEnvKeys() {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
	}
	return env
}

// AgentProcessEnv supplies initial authentication for the adapter's auth probe.
// All other provider settings belong to SDK options, so a long-lived adapter
// cannot pass its initial provider's custom env to sessions using another one.
func AgentProcessEnv(options json.RawMessage) ([]string, error) {
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(options, &parsed); err != nil {
		return nil, err
	}
	transport := make(map[string]string, 3)
	for key, value := range parsed.Env {
		if isProxyTransportEnv(key) {
			transport[key] = value
		}
	}
	env := buildProcessEnv(os.Environ(), settingsJSON{Env: transport}, true)
	env = append(env, "ANTHROPIC_API_KEY="+transport["ANTHROPIC_API_KEY"])
	return env, nil
}

// DefaultAgentLaunch runs ACP sessions on Claude Code's own configuration
// (the "default" selector) exactly as it stands, including a running ccl
// proxy's overlay: no provider runtime, no settings, and every routing key the
// adapter's long-lived process may hold from another provider is put back to
// ccl's own environment, as a plain Claude Code launch would see it.
type DefaultAgentLaunch struct{}

func (DefaultAgentLaunch) Close() {}

// ClaudeAccountAgentLaunch uses Claude's native subscription inside the
// selected profile; protocol and token refresh stay with the upstream SDK.
type ClaudeAccountAgentLaunch struct{ ConfigDir string }

func (ClaudeAccountAgentLaunch) Close() {}

func (l ClaudeAccountAgentLaunch) Options(cwd string) (json.RawMessage, error) {
	env := completeAgentEnv(settingsJSON{Env: map[string]string{}}, false)
	for key := range env {
		env[key] = ""
		if strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
			env[key] = "0"
		}
	}
	env["CLAUDE_CONFIG_DIR"] = l.ConfigDir
	env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = l.ConfigDir
	env["CLAUDE_CODE_OAUTH_TOKEN"] = ""
	env["CLAUDE_CODE_HOST_CREDS_FILE"] = ""
	env["CLAUDE_CODE_HOST_AUTH_ENV_VAR"] = ""
	env["CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"] = ""
	return json.Marshal(struct {
		Env      map[string]string `json:"env"`
		Settings map[string]any    `json:"settings"`
	}{env, map[string]any{"env": env, "apiKeyHelper": ""}})
}

func (DefaultAgentLaunch) Options(cwd string) (json.RawMessage, error) {
	env := completeAgentEnv(settingsJSON{Env: map[string]string{}}, false)
	for key := range env {
		env[key] = os.Getenv(key)
	}
	return json.Marshal(struct {
		Env      map[string]string          `json:"env"`
		Settings map[string]json.RawMessage `json:"settings"`
	}{env, map[string]json.RawMessage{}})
}
