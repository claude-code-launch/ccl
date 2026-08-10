package provider

import (
	"net/url"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

// Claude Code env vars that ccl manages through Provider.Env. They live here so
// the launcher, the config TUI and the diagnostics all agree on the spelling.
const (
	// EnvMaxContextTokens is the fallback context size Claude Code assumes for a
	// model it does not recognize.
	EnvMaxContextTokens = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	// EnvAutoCompactWindow is the window Claude Code uses as the basis for its
	// auto-compact threshold. EnvAutoCompactPct selects the percentage of it.
	EnvAutoCompactWindow = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

	// EnvContextBudgetMode is a retired ccl directive kept only so old provider
	// env maps can remove it instead of forwarding it to Claude Code.
	EnvContextBudgetMode = "CCL_CONTEXT_BUDGET"

	// EnvAutoCompactPct is Claude Code's percentage-based auto-compact threshold.
	// It only ever lowers the trigger point, and Claude Code has repeatedly
	// ignored it when it arrives through the settings file, so ccl also exports it
	// to the child process environment.
	EnvAutoCompactPct = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"

	BalancedMaxContextTokens  = "500000"
	BalancedAutoCompactWindow = "500000"
	BalancedAutoCompactPct    = "80"
)

// ManagedContextEnvKeys are the context-sizing variables ccl forwards. They are
// exported to the Claude Code process as well as written to the settings file,
// because the settings-file channel has proven unreliable for them.
func ManagedContextEnvKeys() []string {
	return []string{EnvMaxContextTokens, EnvAutoCompactWindow, EnvAutoCompactPct}
}

// IsBalancedContextPreset reports whether env contains ccl's one managed
// override: a 500K window compacted at 80% (approximately 400K).
func IsBalancedContextPreset(env map[string]string) bool {
	return strings.TrimSpace(env[EnvMaxContextTokens]) == BalancedMaxContextTokens &&
		strings.TrimSpace(env[EnvAutoCompactWindow]) == BalancedAutoCompactWindow &&
		strings.TrimSpace(env[EnvAutoCompactPct]) == BalancedAutoCompactPct
}

// HasManagedContextEnv reports whether any Claude Code context variable is set.
func HasManagedContextEnv(env map[string]string) bool {
	for _, key := range ManagedContextEnvKeys() {
		if strings.TrimSpace(env[key]) != "" {
			return true
		}
	}
	return false
}

type Provider struct {
	Name string `yaml:"name" mapstructure:"name"`
	// Type selects the upstream protocol for a manual gateway. For OAuth
	// subscriptions it is only the local adapter compatibility type; the real
	// backend and authentication flow are selected by OAuthProvider.
	Type string `yaml:"type" mapstructure:"type"`
	// Endpoint is an HTTP API base for manual gateways and an oauth:// descriptor
	// for persisted subscriptions. Provider Session replaces the latter with a
	// loopback address only in its runtime copy.
	Endpoint string `yaml:"endpoint" mapstructure:"endpoint"`
	APIKey   string `yaml:"apikey" mapstructure:"apikey"`
	// Model is ccl's local model pool used for TUI mapping, slot defaults, and
	// availability checks. For OpenAI-family providers it is also registered as
	// CLIProxyAPI model routes/aliases; direct Anthropic providers must expose
	// their own /v1/models to Claude Code.
	Model string            `yaml:"model" mapstructure:"model"`
	Env   map[string]string `yaml:"env,omitempty" mapstructure:"env,omitempty"`
	// AnthropicAuth controls how Claude Code authenticates direct Anthropic-compatible providers.
	// Empty and "x-api-key" use ANTHROPIC_API_KEY; "bearer" uses ANTHROPIC_AUTH_TOKEN.
	AnthropicAuth string `yaml:"anthropicAuth,omitempty" mapstructure:"anthropicAuth,omitempty"`
	// OAuthProvider selects an embedded subscription runtime. Supported
	// values are gpt, gemini, grok, copilot, qoder, kimi, kiro, and claude. The
	// legacy chatgpt and codex values remain readable.
	OAuthProvider string `yaml:"oauthProvider,omitempty" mapstructure:"oauthProvider,omitempty"`
	// OAuthAccountCredential binds this provider to a single credential file
	// (basename of the JSON under ~/.ccl/auth). Subscription runtimes require
	// this binding and load only that account.
	OAuthAccountCredential string `yaml:"oauthAccountCredential,omitempty" mapstructure:"oauthAccountCredential,omitempty"`

	// Custom model configuration (Claude Code native features)
	CustomModelID  string            `yaml:"customModelId,omitempty" mapstructure:"customModelId,omitempty"`   // ANTHROPIC_CUSTOM_MODEL_OPTION
	OpusModel      string            `yaml:"opusModel,omitempty" mapstructure:"opusModel,omitempty"`           // ANTHROPIC_DEFAULT_OPUS_MODEL
	SonnetModel    string            `yaml:"sonnetModel,omitempty" mapstructure:"sonnetModel,omitempty"`       // ANTHROPIC_DEFAULT_SONNET_MODEL
	HaikuModel     string            `yaml:"haikuModel,omitempty" mapstructure:"haikuModel,omitempty"`         // ANTHROPIC_DEFAULT_HAIKU_MODEL
	SubagentModel  string            `yaml:"subagentModel,omitempty" mapstructure:"subagentModel,omitempty"`   // CLAUDE_CODE_SUBAGENT_MODEL
	ModelOverrides map[string]string `yaml:"modelOverrides,omitempty" mapstructure:"modelOverrides,omitempty"` // modelOverrides in settings.json
	EffortLevel    string            `yaml:"effortLevel,omitempty" mapstructure:"effortLevel,omitempty"`       // CLAUDE_CODE_EFFORT_LEVEL; empty means Default/follow Claude
	// FastMode mirrors the Claude Code settings.json fastMode flag, the same
	// toggle flipped by the `/fast` slash command. It routes ChatGPT/Codex
	// subscription accounts through Codex's faster responses (≈1.5x speed) at
	// the cost of higher usage; only meaningful for the GPT/Codex Responses
	// OAuth backend. Empty/zero leaves Claude Code's own setting.
	FastMode bool `yaml:"fastMode,omitempty" mapstructure:"fastMode,omitempty"`
}

type Config struct {
	ActiveProvider string `yaml:"active_provider" mapstructure:"active_provider"`
	Lang           string `yaml:"lang,omitempty" mapstructure:"lang,omitempty"`
	// BypassMode automatically passes --dangerously-skip-permissions to Claude
	// Code for every ccl-launched session. It is a global launcher setting.
	BypassMode bool `yaml:"bypass_mode,omitempty" mapstructure:"bypass_mode,omitempty"`
	// LogLevel is the threshold for ccl's per-session slog files: debug, info,
	// warn, error, or off. Config loading normalizes an omitted value to off.
	LogLevel string `yaml:"log_level,omitempty" mapstructure:"log_level,omitempty"`
	// DebugMode and DebugVerbose remain readable only to migrate configurations
	// written before `ccl debug` was renamed to `ccl log`.
	DebugMode    bool                `yaml:"debug_mode,omitempty" mapstructure:"debug_mode,omitempty"`
	DebugVerbose bool                `yaml:"debug_verbose,omitempty" mapstructure:"debug_verbose,omitempty"`
	Providers    map[string]Provider `yaml:"providers" mapstructure:"providers"`
}

// OAuthRuntimeType returns the internal compatibility type ccl persists for an
// OAuth backend. Copilot is represented by openai_responses for local dispatch,
// but its actual upstream protocol is selected per model. ok is false when the
// backend is empty or unknown.
func OAuthRuntimeType(oauthProvider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "gpt", "chatgpt", "codex", "copilot":
		return "openai_responses", true
	case "gemini", "grok", "kimi":
		return "openai", true
	case "kiro", "qoder", "claude":
		return "anthropic", true
	default:
		return "", false
	}
}

// InferOAuthProvider restores the public OAuth provider name for configs
// written before oauthProvider was persisted. The oauth:// endpoint is an
// internal backend marker, so ordinary HTTP providers are never inferred.
func InferOAuthProvider(providerName, endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || !strings.EqualFold(u.Scheme, "oauth") {
		return ""
	}

	backend := strings.ToLower(strings.TrimSpace(u.Host))
	switch backend {
	case "codex", "chatgpt", "gpt":
		if strings.EqualFold(strings.TrimSpace(providerName), "codex") {
			return "codex"
		}
		// Prefer public name "gpt"; keep "chatgpt" only when the provider key itself is legacy.
		if strings.EqualFold(strings.TrimSpace(providerName), "chatgpt") {
			return "chatgpt"
		}
		return "gpt"
	case "antigravity", "gemini":
		return "gemini"
	case "copilot":
		return "copilot"
	case "qoder":
		return "qoder"
	case "xai", "grok":
		return "grok"
	case "kimi":
		return "kimi"
	case "kiro":
		return "kiro"
	case "claude":
		return "claude"
	default:
		return ""
	}
}

func IsOpenAIResponsesType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai_responses" ||
		providerType == "openai-responses" ||
		providerType == "responses" ||
		providerType == "openai(responses)" ||
		providerType == "openai(agent)"
}

func IsOpenAICompatibleType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai" ||
		providerType == "openai(chat)" ||
		IsOpenAIResponsesType(providerType)
}

func IsAnthropicType(providerType string) bool {
	return strings.EqualFold(strings.TrimSpace(providerType), "anthropic")
}

// RuntimeModelSpec returns every model ID that Claude Code may send for this
// provider. Embedded runtimes use the list to register model routes and aliases.
func RuntimeModelSpec(p Provider) string {
	models := make([]string, 0)
	seen := make(map[string]bool)
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if seen[key] {
			return
		}
		seen[key] = true
		models = append(models, model)
	}
	for _, model := range modelrouting.SplitCSV(p.Model) {
		add(model)
	}
	for _, model := range []string{
		p.CustomModelID,
		p.OpusModel,
		p.SonnetModel,
		p.HaikuModel,
		p.SubagentModel,
	} {
		add(model)
	}
	overrideKeys := make([]string, 0, len(p.ModelOverrides))
	for key := range p.ModelOverrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		add(p.ModelOverrides[key])
	}
	return strings.Join(models, ",")
}

// ProtocolLabel returns a short, human-friendly protocol name for display purposes
// (e.g. in the `set` TUI, `ccl ls`, and `ccl doctor` output). It intentionally does
// NOT change the underlying stored provider.Type value, which remains a stable,
// machine-readable string ("anthropic", "openai", "openai_responses", ...) relied on
// throughout the codebase for dispatch logic (proxy, launcher, doctor, ...).
//
// OpenAI exposes two distinct generation protocols behind the same "openai" umbrella:
//  1. Chat Completions — the old standard, broadest compatibility: labeled "openai(chat)".
//  2. Responses — the newer agent protocol: labeled "openai(responses)".
func ProtocolLabel(providerType string) string {
	trimmed := strings.TrimSpace(providerType)
	switch {
	case trimmed == "":
		return ""
	case IsOpenAIResponsesType(trimmed):
		return "openai(responses)"
	case IsAnthropicType(trimmed):
		return "anthropic"
	default:
		return "openai(chat)"
	}
}

// ProtocolLabelForProvider reports the user-facing protocol, including OAuth
// backends whose real behavior cannot be inferred from the internal Type field.
func ProtocolLabelForProvider(p Provider) string {
	if strings.EqualFold(strings.TrimSpace(p.OAuthProvider), "copilot") {
		return "copilot(auto)"
	}
	return ProtocolLabel(p.Type)
}
