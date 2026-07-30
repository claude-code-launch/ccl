package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
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
	// ccl directives are not Claude Code variables.
	removeEnvKey(env, provider.EnvContextBudgetMode)
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

// suppressedProcessEnvKey reports whether an inherited key is one ccl owns
// through the settings file, honoring Windows' case-insensitive names.
func suppressedProcessEnvKey(suppressed map[string]struct{}, key string) (string, bool) {
	if _, ok := suppressed[key]; ok {
		return key, true
	}
	for candidate := range suppressed {
		if sameEnvKey(candidate, key) {
			return candidate, true
		}
	}
	return "", false
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
// per-session endpoint and bearer token used by the embedded proxy, and exports
// the context-sizing variables ccl manages.
//
// Those are also written to the settings file, but Claude Code has been reported
// to ignore auto-compact settings that only arrive that way, honoring them only
// when they are present in the environment. Exporting them costs nothing and
// removes that failure mode.
func buildProcessEnv(inherited []string, settings settingsJSON, useProxy bool) []string {
	// The settings file is authoritative for everything ccl configures, so an
	// inherited copy of one of those keys is dropped rather than left to compete
	// with it. The context keys are dropped too, even though ccl no longer writes
	// them: a value exported in the user's shell would otherwise silently reinstate
	// the session-wide window that ccl deliberately stopped declaring. ccl's own
	// CCL_* variables, and everything unrelated, are inherited untouched.
	suppressed := make(map[string]struct{}, len(settings.Env)+4)
	for key := range settings.Env {
		suppressed[key] = struct{}{}
	}
	for _, key := range provider.ManagedContextEnvKeys() {
		suppressed[key] = struct{}{}
	}
	exported := make(map[string]string, 4)
	for _, key := range provider.ManagedContextEnvKeys() {
		if value := strings.TrimSpace(settings.Env[key]); value != "" {
			exported[key] = value
		}
	}

	env := make([]string, 0, len(inherited)+len(exported)+2)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if useProxy && isProxyTransportEnv(key) {
			continue
		}
		if _, drop := suppressedProcessEnvKey(suppressed, key); drop {
			oauthproxy.Debugf("launcher ignores inherited %s: the settings file is authoritative", key)
			continue
		}
		env = append(env, entry)
	}
	// Claude Code has been reported to ignore auto-compact settings that arrive
	// only through the settings file, so the ones ccl does set are also exported.
	for _, key := range provider.ManagedContextEnvKeys() {
		if value, ok := exported[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	if !useProxy {
		return env
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

// newSessionName returns the identifier shared by this session's settings file
// and its debug log. It is generated before anything else runs so the very first
// log line already lands in the per-session file.
func newSessionName() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "claude_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "claude_" + hex.EncodeToString(raw)
}

// writeSettingsFile serialises content to a JSON file named after the session and
// returns its path. The caller is responsible for removing the file when done.
func writeSettingsFile(content settingsJSON, session string) (string, error) {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal settings: %w", err)
	}

	path := filepath.Join(os.TempDir(), session+"_settings.json")
	// O_EXCL: never reuse or overwrite another session's settings file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create settings file: %w", err)
	}

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
	// droppedContextPreset records that a context preset from an older ccl version
	// was removed from this session's env.
	droppedContextPreset bool
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
			Protocol:                upstreamProtocol,
			Endpoint:                providerCopy.Endpoint,
			APIKey:                  providerCopy.APIKey,
			ModelSpec:               provider.RuntimeModelSpec(providerCopy),
			OAuthProvider:           providerCopy.OAuthProvider,
			OAuthAccountCredential:  providerCopy.OAuthAccountCredential,
			OAuthAccountCredentials: providerCopy.OAuthAccountCredentials,
			OAuthCredentialResolver: groupCredentialResolver(providerCopy.AuthGroup),
			MaxOutputTokens:         maxOut,
		})
		if err != nil {
			return nil, fmt.Errorf("start embedded provider runtime: %w", err)
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

func groupCredentialResolver(groupName string) func() ([]string, error) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return nil
	}
	return func() ([]string, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		group, ok := cfg.AuthGroups[groupName]
		if !ok {
			return []string{}, nil
		}
		return append([]string{}, group.Credentials...), nil
	}
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
			return fmt.Errorf("discover embedded provider runtime models: %w", err)
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
	env := buildEnv(c.provider, c.baseURL, c.useProxy)
	// Applied after the provider Env overrides: a preset an older ccl stored must
	// not survive into a session Claude Code should size itself.
	c.droppedContextPreset = applyContextPolicy(env, c.provider)
	return settingsJSON{
		Env:                    env,
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
		oauthproxy.Debugf("claude CLI not found in PATH: %v", err)
		return fmt.Errorf("claude CLI not found in PATH (install with: npm install -g @anthropic-ai/claude-code): %w", err)
	}

	// Give this session its own debug log before the embedded runtime starts, so
	// every line (runtime startup included) belongs to one readable file.
	session := newSessionName()
	if oauthproxy.DebugEnabled() {
		oauthproxy.SetDebug(true, oauthproxy.SessionDebugLogPath(session))
		oauthproxy.Debugf("session start name=%q provider=%q oauth=%q", session, p.Name, p.OAuthProvider)
	}

	ctx, err := setupProvider(p)
	if err != nil {
		oauthproxy.Debugf("session setup failed name=%q provider=%q oauth=%q error=%v", session, p.Name, p.OAuthProvider, err)
		return err
	}
	defer ctx.cleanup()

	sessionSettings := ctx.settings()
	settingsPath, err := writeSettingsFile(sessionSettings, session)
	if err != nil {
		oauthproxy.Debugf("session settings write failed name=%q error=%v", session, err)
		return fmt.Errorf("create settings file: %w", err)
	}
	defer os.Remove(settingsPath)
	logSessionContextBudget(p, sessionSettings, ctx.droppedContextPreset)

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

	start := time.Now()
	runErr := cmd.Run()

	// Token usage, unlike the debug log, is printed unconditionally: it is
	// information about what the session cost, not a diagnostic. ctx.oauth is
	// nil for providers that never start an embedded runtime (plain Anthropic
	// endpoints), and Usage()/Snapshot() are nil-safe, so this is a no-op for
	// them rather than a special case here.
	if summary := oauthproxy.FormatUsageSummary(usageSnapshot(ctx.oauth)); summary != "" {
		fmt.Fprintln(os.Stderr, "\n"+summary)
	}

	// Approximate session metadata for the debug log. These are ccl-side counts,
	// not the real upstream request size, and never include credentials or
	// request bodies. Useful to correlate with upstream errors logged by CPA.
	if oauthproxy.DebugEnabled() {
		spec := provider.RuntimeModelSpec(p)
		modelCount := 0
		if spec != "" {
			modelCount = len(strings.Split(spec, ","))
		}
		outcome := "ok"
		if runErr != nil {
			outcome = runErr.Error()
		}
		oauthproxy.Debugf("launcher exit provider=%q oauth=%q protocol=%q base=%q use_proxy=%t model_count=%d env_override=%d custom_model=%q fast=%t outcome=%s duration=%s",
			p.Name, p.OAuthProvider, provider.ProtocolLabel(p.Type), ctx.baseURL,
			ctx.useProxy, modelCount, len(p.Env), p.CustomModelID, p.FastMode,
			outcome, time.Since(start).Round(time.Millisecond))
	}
	return runErr
}

// usageSnapshot returns the accumulated per-model token usage of an embedded
// runtime, or nil when there is none (a plain Anthropic provider never starts
// one, so this is the normal case for that provider type).
func usageSnapshot(runtime *oauthproxy.Runtime) []oauthproxy.UsageModelTotals {
	totals, ok := runtime.Usage().Snapshot()
	if !ok {
		return nil
	}
	return totals
}

// logSessionContextBudget records the context limits handed to Claude Code plus
// the slot mapping they apply to.
//
// An "input exceeds the context window" failure mid-session is otherwise
// invisible in the log: the upstream rejects the request because Claude Code was
// told it could grow further than the backend allows, and nothing else records
// which numbers were in effect. Compare these against `ccl doctor` →
// "Context budget", which reads the window the backend advertises.
func logSessionContextBudget(p provider.Provider, settings settingsJSON, droppedPreset bool) {
	if !oauthproxy.DebugEnabled() {
		return
	}
	// The [1m] suffix is what decides the session sizing, so log the raw slot
	// values rather than the stripped model ids.
	mapped := make([]string, 0, 5)
	for _, slot := range []struct{ name, model string }{
		{"opus", p.OpusModel},
		{"sonnet", p.SonnetModel},
		{"haiku", p.HaikuModel},
		{"custom", p.CustomModelID},
		{"subagent", p.SubagentModel},
	} {
		if strings.TrimSpace(slot.model) != "" {
			mapped = append(mapped, slot.name+"="+slot.model)
		}
	}
	oauthproxy.Debugf("launcher context budget provider=%q oauth=%q max_context_tokens=%q auto_compact_window=%q auto_compact_pct=%q dropped_ccl_preset=%t manual=%t effort=%q max_output=%q slots=[%s]",
		p.Name, p.OAuthProvider,
		settings.Env[provider.EnvMaxContextTokens],
		settings.Env[provider.EnvAutoCompactWindow],
		settings.Env[provider.EnvAutoCompactPct],
		droppedPreset, provider.ContextBudgetIsManual(p),
		settings.Env["CLAUDE_CODE_EFFORT_LEVEL"],
		settings.Env[MaxOutputTokensEnv],
		strings.Join(mapped, " "))
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
