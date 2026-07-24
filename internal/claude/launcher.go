package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// ─────────────────────────────────────────────────────────────────────────────
// Settings file
// ─────────────────────────────────────────────────────────────────────────────

// settingsJSON is the per-session settings file consumed by the Claude CLI (--settings).
type settingsJSON struct {
	Env                    map[string]string `json:"env"`
	HasCompletedOnboarding bool              `json:"hasCompletedOnboarding"`
	Model                  string            `json:"model,omitempty"`
	ModelOverrides         map[string]string `json:"modelOverrides,omitempty"` // Map standard IDs to provider-specific IDs
	// FastMode is always serialized (no omitempty) so turning it off in ccl set
	// (or Claude Code /fast) can clear a previously enabled pin.
	FastMode bool `json:"fastMode"`
}

const (
	SubagentModelEnv          = "CLAUDE_CODE_SUBAGENT_MODEL"
	ToolUseConcurrencyEnv     = "CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY"
	ToolSearchEnv             = "ENABLE_TOOL_SEARCH"
	MaxOutputTokensEnv        = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"
	DefaultToolUseConcurrency = "3"
	DefaultToolSearch         = "false"
	DefaultMaxOutputTokens    = "32000"
	MaxOutputTokensUpperLimit = 128000
)

// RuntimeSettings are ccl's Claude Code process defaults. Provider Env values
// override these defaults so advanced users retain an escape hatch.
type RuntimeSettings struct {
	SubagentModel      string
	ToolUseConcurrency string
	ToolSearch         string
	MaxOutputTokens    string
}

func ResolveRuntimeSettings(p provider.Provider) RuntimeSettings {
	subagentModel := strings.TrimSpace(p.SubagentModel)
	if subagentModel == "" {
		subagentModel = defaultSubagentModel(p)
	}
	settings := RuntimeSettings{
		SubagentModel:      subagentModel,
		ToolUseConcurrency: DefaultToolUseConcurrency,
		ToolSearch:         DefaultToolSearch,
		MaxOutputTokens:    DefaultMaxOutputTokens,
	}
	if value, ok := p.Env[SubagentModelEnv]; ok {
		settings.SubagentModel = value
	}
	if value, ok := p.Env[ToolUseConcurrencyEnv]; ok {
		settings.ToolUseConcurrency = value
	}
	if value, ok := p.Env[ToolSearchEnv]; ok {
		settings.ToolSearch = value
	}
	if value, ok := p.Env[MaxOutputTokensEnv]; ok {
		if normalized, err := NormalizeMaxOutputTokens(value); err == nil {
			settings.MaxOutputTokens = normalized
		}
	}
	return settings
}

// NormalizeMaxOutputTokens validates Claude Code's per-response output cap.
// Context window sizes such as 200K or 1M are separate settings and must not
// be used here.
func NormalizeMaxOutputTokens(value string) (string, error) {
	value = strings.TrimSpace(value)
	tokens, err := strconv.Atoi(value)
	if err != nil || tokens < 1 || tokens > MaxOutputTokensUpperLimit {
		return "", fmt.Errorf("must be an integer between 1 and %d", MaxOutputTokensUpperLimit)
	}
	return strconv.Itoa(tokens), nil
}

func defaultSubagentModel(p provider.Provider) string {
	if model := strings.TrimSpace(p.CustomModelID); model != "" {
		return model
	}
	if model := strings.TrimSpace(p.SonnetModel); model != "" {
		return model
	}
	models := modelrouting.SplitCSV(p.Model)
	if len(models) == 0 {
		return ""
	}
	return modelrouting.MapModel("claude-3-5-sonnet", "", models)
}

// buildEnv constructs the env-var overrides for a settings file.
func buildEnv(p provider.Provider, baseURL string, useProxy bool) map[string]string {
	env := make(map[string]string)

	if baseURL != "" {
		if !useProxy && provider.IsAnthropicType(p.Type) {
			baseURL = protocol.NormalizeAnthropicBaseURLForClaude(baseURL)
		}
		env["ANTHROPIC_BASE_URL"] = baseURL
	}

	switch {
	case useProxy:
		env["ANTHROPIC_AUTH_TOKEN"] = p.APIKey
	case p.APIKey != "":
		if provider.IsAnthropicType(p.Type) && strings.EqualFold(p.AnthropicAuth, "bearer") {
			env["ANTHROPIC_AUTH_TOKEN"] = p.APIKey
		} else {
			env["ANTHROPIC_API_KEY"] = p.APIKey
		}
	}

	// 1. Custom model option shown as the persistent "Custom model" row in /model.
	// Technical IDs may keep the [1m] suffix; *_NAME is display-only.
	if p.CustomModelID != "" {
		env["ANTHROPIC_CUSTOM_MODEL_OPTION"] = p.CustomModelID
		env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"] = modelDisplayName(p.CustomModelID)
		env["CLAUDE_CODE_MODEL_ID"] = p.CustomModelID
	}

	// 2. Explicit tier model overrides (user-specified)
	if p.OpusModel != "" {
		env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = p.OpusModel
		env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] = modelDisplayName(p.OpusModel)
	}
	if p.SonnetModel != "" {
		env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = p.SonnetModel
		env["ANTHROPIC_DEFAULT_SONNET_MODEL_NAME"] = modelDisplayName(p.SonnetModel)
	}
	if p.HaikuModel != "" {
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = p.HaikuModel
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] = modelDisplayName(p.HaikuModel)
	}

	// 3. Effort level; empty means ccl leaves Claude's own setting in control.
	if p.EffortLevel != "" {
		env["CLAUDE_CODE_EFFORT_LEVEL"] = p.EffortLevel
	}

	// 4. Model pool routing (auto-assign tiers from comma-separated list)
	// Only used as fallback when explicit tier models aren't set
	if p.Model != "" && (p.OpusModel == "" || p.SonnetModel == "" || p.HaikuModel == "") {
		applyModelEnv(env, p.Model)
	}

	// Gateway discovery & traffic reduction (always enabled for multi-model setups)
	if p.Model != "" || p.CustomModelID != "" {
		env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	}

	// Claude Code runtime defaults. Subagents use the explicit mapping when set
	// and otherwise follow the effective main model. Provider-level Env values
	// below can override every default.
	runtimeSettings := ResolveRuntimeSettings(p)
	if runtimeSettings.SubagentModel != "" {
		env[SubagentModelEnv] = runtimeSettings.SubagentModel
	}
	env[ToolUseConcurrencyEnv] = runtimeSettings.ToolUseConcurrency
	env[ToolSearchEnv] = runtimeSettings.ToolSearch
	env[MaxOutputTokensEnv] = runtimeSettings.MaxOutputTokens

	// Provider-level overrides take final precedence except for embedded-proxy
	// transport values, which must match the runtime started for this session.
	for k, v := range p.Env {
		env[k] = v
	}
	if useProxy {
		removeEnvKey(env, "ANTHROPIC_API_KEY")
		removeEnvKey(env, "ANTHROPIC_BASE_URL")
		removeEnvKey(env, "ANTHROPIC_AUTH_TOKEN")
		env["ANTHROPIC_BASE_URL"] = baseURL
		env["ANTHROPIC_AUTH_TOKEN"] = p.APIKey
	}
	// Keep this safety-critical value validated even when an older config
	// contains an invalid context-window-sized override.
	env[MaxOutputTokensEnv] = runtimeSettings.MaxOutputTokens
	return env
}

func sameEnvKey(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func removeEnvKey(env map[string]string, key string) {
	for existing := range env {
		if sameEnvKey(existing, key) {
			delete(env, existing)
		}
	}
}

func isProxyTransportEnv(key string) bool {
	return sameEnvKey(key, "ANTHROPIC_API_KEY") ||
		sameEnvKey(key, "ANTHROPIC_AUTH_TOKEN") ||
		sameEnvKey(key, "ANTHROPIC_BASE_URL")
}

// buildProcessEnv prevents ambient Anthropic credentials from overriding the
// per-session endpoint and bearer token used by the embedded proxy.
func buildProcessEnv(inherited []string, settings settingsJSON, useProxy bool) []string {
	if !useProxy {
		return inherited
	}

	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isProxyTransportEnv(key) {
			continue
		}
		env = append(env, entry)
	}
	if value := settings.Env["ANTHROPIC_BASE_URL"]; value != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+value)
	}
	if value := settings.Env["ANTHROPIC_AUTH_TOKEN"]; value != "" {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+value)
	}
	return env
}

// applyModelEnv writes model-related env vars into env.
// A comma-separated model spec enables per-tier gateway routing;
// a single name fills every missing tier and ANTHROPIC_MODEL with that model.
func applyModelEnv(env map[string]string, modelSpec string) {
	setIfEmpty := func(key, value string) {
		if value == "" {
			return
		}
		if _, ok := env[key]; !ok {
			env[key] = value
		}
	}

	if !strings.Contains(modelSpec, ",") {
		model := strings.TrimSpace(modelSpec)
		setIfEmpty("ANTHROPIC_DEFAULT_OPUS_MODEL", model)
		setIfEmpty("ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", modelDisplayName(model))
		setIfEmpty("ANTHROPIC_DEFAULT_SONNET_MODEL", model)
		setIfEmpty("ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", modelDisplayName(model))
		setIfEmpty("ANTHROPIC_DEFAULT_HAIKU_MODEL", model)
		setIfEmpty("ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", modelDisplayName(model))
		setIfEmpty("ANTHROPIC_MODEL", env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
		return
	}

	models := modelrouting.SplitCSV(modelSpec)
	opus := modelrouting.MapModel("claude-3-opus", "", models)
	sonnet := modelrouting.MapModel("claude-3-5-sonnet", "", models)
	haiku := modelrouting.MapModel("claude-3-5-haiku", "", models)

	setIfEmpty("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1")
	setIfEmpty("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")

	for _, kv := range []struct{ k, v string }{
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", opus},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", modelDisplayName(opus)},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL", sonnet},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", modelDisplayName(sonnet)},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL", haiku},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", modelDisplayName(haiku)},
	} {
		setIfEmpty(kv.k, kv.v)
	}
	setIfEmpty("ANTHROPIC_MODEL", env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
}

// writeSettingsFile serialises content to a temp JSON file and returns its path.
// The caller is responsible for removing the file when done.
func writeSettingsFile(content settingsJSON) (string, error) {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal settings: %w", err)
	}

	f, err := os.CreateTemp("", "claude_*_settings.json")
	if err != nil {
		return "", fmt.Errorf("create temp settings file: %w", err)
	}
	path := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close settings file: %w", err)
	}
	return path, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Provider context — shared setup for PreviewSettings and Run
// ─────────────────────────────────────────────────────────────────────────────

// providerContext holds resolved state needed to build a settings file.
type providerContext struct {
	provider provider.Provider // copy, not reference — safe to mutate
	baseURL  string
	useProxy bool
	oauth    *oauthproxy.Runtime
}

// setupProvider starts a proxy if needed and resolves the final model list.
// The caller must call cleanup() to release any proxy resources.
func setupProvider(p provider.Provider) (*providerContext, error) {
	// Make a COPY to avoid mutating the original provider (fixes mutation bug)
	providerCopy := p
	// OpenAI-family providers and all OAuth backends (including Claude OAuth)
	// go through the embedded CPA runtime so Claude Code always hits a local
	// /v1/messages endpoint with a session token.
	useProxy := provider.IsOpenAICompatibleType(p.Type) || strings.TrimSpace(p.OAuthProvider) != ""
	ctx := &providerContext{provider: providerCopy, useProxy: useProxy}
	if ctx.useProxy {
		if providerCopy.OAuthProvider == "" && strings.TrimSpace(providerCopy.Model) == "" {
			models, err := protocol.GetOpenAIModels(providerCopy.Endpoint, providerCopy.APIKey)
			if err != nil {
				return nil, fmt.Errorf("discover OpenAI models before starting CLIProxyAPI: %w", err)
			}
			providerCopy.Model = models
		}
		upstreamProtocol := oauthproxy.ProtocolOpenAIChat
		if provider.IsOpenAIResponsesType(providerCopy.Type) {
			upstreamProtocol = oauthproxy.ProtocolOpenAIResponses
		}
		maxOut := 0
		if provider.IsOpenAIResponsesType(providerCopy.Type) {
			if n, err := strconv.Atoi(ResolveRuntimeSettings(providerCopy).MaxOutputTokens); err == nil {
				maxOut = n
			}
		}
		runtime, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
			Protocol:               upstreamProtocol,
			Endpoint:               providerCopy.Endpoint,
			APIKey:                 providerCopy.APIKey,
			ModelSpec:              provider.RuntimeModelSpec(providerCopy),
			OAuthProvider:          providerCopy.OAuthProvider,
			OAuthAccountCredential: providerCopy.OAuthAccountCredential,
			MaxOutputTokens:        maxOut,
		})
		if err != nil {
			return nil, fmt.Errorf("start embedded CLIProxyAPI: %w", err)
		}
		ctx.oauth = runtime
		providerCopy.Endpoint = runtime.Endpoint()
		providerCopy.APIKey = runtime.APIKey()
		ctx.provider = providerCopy
		ctx.baseURL = runtime.ClaudeBaseURL()
	} else {
		ctx.baseURL = providerCopy.Endpoint
	}

	if err := ctx.resolveModel(); err != nil {
		ctx.cleanup()
		return nil, err
	}
	return ctx, nil
}

// resolveModel seeds preferred OAuth slot defaults for empty tiers, discovers
// the model list when none is configured, then drops preferred defaults that
// are absent from the live catalog so auto-mapping can fill those tiers.
// Mutates the local copy only.
func (c *providerContext) resolveModel() error {
	// Apply first so existing Grok providers without saved slot pins still get
	// the preferred mapping before catalog validation.
	provider.ApplyOAuthSlotDefaults(&c.provider)
	if c.provider.Model == "" && c.oauth != nil {
		models, err := protocol.GetOpenAIModels(c.provider.Endpoint, c.provider.APIKey)
		if err != nil {
			return fmt.Errorf("discover embedded CLIProxyAPI models: %w", err)
		}
		c.provider.Model = models
	}
	if c.provider.Model != "" {
		provider.ClearUnavailablePreferredDefaults(&c.provider, modelrouting.SplitCSV(c.provider.Model))
	}
	return nil
}

func (c *providerContext) cleanup() {
	if c.oauth != nil {
		c.oauth.Stop()
	}
}

func (c *providerContext) settings() settingsJSON {
	return settingsJSON{
		Env:                    buildEnv(c.provider, c.baseURL, c.useProxy),
		HasCompletedOnboarding: true,
		Model:                  c.provider.CustomModelID,
		ModelOverrides:         c.provider.ModelOverrides,
		FastMode:               c.provider.FastMode,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

// PreviewSettings returns the JSON that would be written to the settings temp file.
func PreviewSettings(p provider.Provider) string {
	ctx, err := setupProvider(p)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer ctx.cleanup()

	data, err := json.MarshalIndent(ctx.settings(), "", "  ")
	if err != nil {
		return fmt.Sprintf("Error: marshal settings: %v", err)
	}
	return string(data)
}

// Run launches the Claude CLI with settings derived from p, forwarding extra args.
func Run(p provider.Provider, args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH (install with: npm install -g @anthropic-ai/claude-code): %w", err)
	}

	ctx, err := setupProvider(p)
	if err != nil {
		return err
	}
	defer ctx.cleanup()

	sessionSettings := ctx.settings()
	settingsPath, err := writeSettingsFile(sessionSettings)
	if err != nil {
		return fmt.Errorf("create settings file: %w", err)
	}
	defer os.Remove(settingsPath)

	fmt.Println("Using provider-specific claude config:", settingsPath)

	claudeArgs := append([]string{"--settings", settingsPath}, args...)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/c", claudePath}, claudeArgs...)...)
	} else {
		cmd = exec.Command(claudePath, claudeArgs...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = buildProcessEnv(os.Environ(), sessionSettings, ctx.useProxy)

	return cmd.Run()
}

// modelDisplayName is the human-facing label for Claude Code *_NAME env vars.
// Technical model IDs may keep the [1m] suffix; display names use " (1M)".
func modelDisplayName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	base := model
	for strings.HasSuffix(base, "[1m]") {
		base = strings.TrimSpace(strings.TrimSuffix(base, "[1m]"))
	}
	if base != model {
		return base + " (1M)"
	}
	return model
}
