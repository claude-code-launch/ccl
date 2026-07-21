package provider

import (
	"net/url"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

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
	// OAuthProvider selects an embedded CLIProxyAPI OAuth backend. Supported
	// values are chatgpt, gemini, and grok. The legacy codex value remains readable.
	OAuthProvider string `yaml:"oauthProvider,omitempty" mapstructure:"oauthProvider,omitempty"`
	// OAuthAccountCredential binds this provider to a single credential file
	// (basename of the JSON under ~/.ccl/auth). The OAuth runtime loads only
	// that account when set; empty falls back to all backend credentials.
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
	// the cost of higher usage; only meaningful for OpenAI Responses OAuth
	// backends (chatgpt/copilot). Empty/zero leaves Claude Code's own setting.
	FastMode bool `yaml:"fastMode,omitempty" mapstructure:"fastMode,omitempty"`
}

type Config struct {
	ActiveProvider string              `yaml:"active_provider" mapstructure:"active_provider"`
	Lang           string              `yaml:"lang,omitempty" mapstructure:"lang,omitempty"`
	Providers      map[string]Provider `yaml:"providers" mapstructure:"providers"`
}

// FixedOAuthProtocol returns the only protocol an OAuth backend actually uses.
// ChatGPT/Codex/Copilot → openai_responses; Gemini and Grok → openai (chat).
// ok is false when oauthProvider is empty or unknown.
func FixedOAuthProtocol(oauthProvider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(oauthProvider)) {
	case "chatgpt", "codex", "copilot":
		return "openai_responses", true
	case "gemini", "grok":
		return "openai", true
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
	case "codex", "chatgpt":
		if strings.EqualFold(strings.TrimSpace(providerName), "codex") {
			return "codex"
		}
		return "chatgpt"
	case "antigravity", "gemini":
		return "gemini"
	case "xai", "grok":
		return "grok"
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
