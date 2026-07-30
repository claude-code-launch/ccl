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
	// EnvAutoCompactWindow is the absolute token count at which Claude Code
	// auto-compacts the conversation.
	EnvAutoCompactWindow = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

	// EnvContextBudgetMode is a ccl directive, not a Claude Code variable: it
	// selects who owns the two limits above for a subscription provider.
	//
	//	auto   (default) follow the window the backend advertises
	//	manual keep the configured values, even when they are larger
	//
	// The advertised number can itself be a client-side catalog cap rather than
	// the server's real limit, so "manual" exists to let a larger window be tried.
	EnvContextBudgetMode = "CCL_CONTEXT_BUDGET"

	// ContextBudgetManual is the EnvContextBudgetMode value that disables
	// backend-driven context management.
	ContextBudgetManual = "manual"

	// EnvAutoCompactPct is Claude Code's percentage-based auto-compact threshold.
	// It only ever lowers the trigger point, and Claude Code has repeatedly
	// ignored it when it arrives through the settings file, so ccl also exports it
	// to the child process environment.
	EnvAutoCompactPct = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
)

// ManagedContextEnvKeys are the context-sizing variables ccl forwards. They are
// exported to the Claude Code process as well as written to the settings file,
// because the settings-file channel has proven unreliable for them.
func ManagedContextEnvKeys() []string {
	return []string{EnvMaxContextTokens, EnvAutoCompactWindow, EnvAutoCompactPct}
}

// cclContextPresets are the (max context, auto-compact window) pairs that older
// ccl versions wrote for their 300K/500K/1M compact presets, and the even older
// percentage-based pairs.
//
// ccl no longer declares context sizes: Claude Code offers a 200K default and a
// per-slot 1M variant, and it sizes its own compaction buffer for whichever
// applies. A leftover global preset only breaks that, so these exact pairs are
// recognized in order to drop them, while any other value is treated as a
// deliberate manual setting and preserved.
var cclContextPresets = [][3]string{
	{"1000000", "900000", ""},
	{"500000", "400000", ""},
	{"300000", "200000", ""},
	{"", "1000000", "90"},
	{"", "500000", "80"},
	{"", "200000", "70"},
	{"", "1000000", ""},
}

// IsCclContextPreset reports whether env holds one of the context presets a
// previous ccl version wrote, rather than values the user chose.
func IsCclContextPreset(env map[string]string) bool {
	if len(env) == 0 {
		return false
	}
	actual := [3]string{
		strings.TrimSpace(env[EnvMaxContextTokens]),
		strings.TrimSpace(env[EnvAutoCompactWindow]),
		strings.TrimSpace(env[EnvAutoCompactPct]),
	}
	if actual == [3]string{"", "", ""} {
		return false
	}
	for _, preset := range cclContextPresets {
		if actual == preset {
			return true
		}
	}
	return false
}

// ContextBudgetIsManual reports whether the provider opted out of backend-driven
// context management.
func ContextBudgetIsManual(p Provider) bool {
	return strings.EqualFold(strings.TrimSpace(p.Env[EnvContextBudgetMode]), ContextBudgetManual)
}

type Provider struct {
	Name     string `yaml:"name" mapstructure:"name"`
	Type     string `yaml:"type" mapstructure:"type"`
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
	// values are gpt, gemini, grok, copilot, kimi, kiro, and claude. The legacy chatgpt
	// codex value remains readable.
	OAuthProvider string `yaml:"oauthProvider,omitempty" mapstructure:"oauthProvider,omitempty"`
	// OAuthAccountCredential binds this provider to a single credential file
	// (basename of the JSON under ~/.ccl/auth). The OAuth runtime loads only
	// that account when set; empty falls back to all backend credentials.
	OAuthAccountCredential string `yaml:"oauthAccountCredential,omitempty" mapstructure:"oauthAccountCredential,omitempty"`
	// AuthGroup points at Config.AuthGroups. Group providers keep their model
	// mapping here while config.Load hydrates OAuthAccountCredentials from the
	// latest group membership before each command/launch.
	AuthGroup string `yaml:"authGroup,omitempty" mapstructure:"authGroup,omitempty"`
	// OAuthAccountCredentials is runtime-only. A non-nil slice means the OAuth
	// runtime must load exactly these files; an empty non-nil slice is an empty
	// group and must never fall back to every account on the backend.
	OAuthAccountCredentials []string `yaml:"-" mapstructure:"-"`

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
	// the cost of higher usage; only meaningful for OpenAI Responses OAuth
	// backends (gpt/copilot). Empty/zero leaves Claude Code's own setting.
	FastMode bool `yaml:"fastMode,omitempty" mapstructure:"fastMode,omitempty"`
}

type Config struct {
	ActiveProvider string `yaml:"active_provider" mapstructure:"active_provider"`
	Lang           string `yaml:"lang,omitempty" mapstructure:"lang,omitempty"`
	// BypassMode automatically passes --dangerously-skip-permissions to Claude
	// Code for every ccl-launched session. It is a global launcher setting.
	BypassMode bool `yaml:"bypass_mode,omitempty" mapstructure:"bypass_mode,omitempty"`
	// DebugMode enables ccl runtime diagnostics for ccl-launched sessions:
	// runtime startup, upstream HTTP status, OAuth refresh, request metadata.
	// Logs to /tmp/ccl-debug.log (override with CCL_DEBUG_LOG). It never logs
	// credentials, refresh tokens, or request/response bodies.
	DebugMode  bool                 `yaml:"debug_mode,omitempty" mapstructure:"debug_mode,omitempty"`
	Providers  map[string]Provider  `yaml:"providers" mapstructure:"providers"`
	AuthGroups map[string]AuthGroup `yaml:"auth_groups,omitempty" mapstructure:"auth_groups,omitempty"`
}

// AuthGroup is a homogeneous pool of OAuth credentials. Credentials contains
// canonical basenames under ~/.ccl/auth; models and Claude slot mappings live
// on the generated group Provider instead of being repeated per token.
type AuthGroup struct {
	OAuthProvider string   `yaml:"oauthProvider" mapstructure:"oauthProvider"`
	Credentials   []string `yaml:"credentials" mapstructure:"credentials"`
}

// FixedOAuthProtocol returns the public protocol label ccl persists for an
// OAuth backend. GPT/Codex/Copilot → Responses; Gemini/Grok/Kimi → OpenAI
// Chat; Kiro/Claude → Anthropic. ok is false when oauthProvider is empty or unknown.
func FixedOAuthProtocol(oauthProvider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "gpt", "chatgpt", "codex", "copilot":
		return "openai_responses", true
	case "gemini", "grok", "kimi":
		return "openai", true
	case "kiro", "claude":
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
