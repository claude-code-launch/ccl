package provider

import (
	"net/url"
	"sort"
	"strings"
)

type Provider struct {
	Name     string `yaml:"name" mapstructure:"name"`
	Type     string `yaml:"type" mapstructure:"type"`
	Endpoint string `yaml:"endpoint" mapstructure:"endpoint"`
	APIKey   string `yaml:"apikey" mapstructure:"apikey"`
	// Model is ccl's local model pool. In OpenAI-compatible proxy mode it is
	// also used to serve /v1/models from the local proxy; direct Anthropic
	// providers must expose their own /v1/models to Claude Code.
	Model string            `yaml:"model" mapstructure:"model"`
	Env   map[string]string `yaml:"env,omitempty" mapstructure:"env,omitempty"`
	// AnthropicAuth controls how Claude Code authenticates direct Anthropic-compatible providers.
	// Empty and "x-api-key" use ANTHROPIC_API_KEY; "bearer" uses ANTHROPIC_AUTH_TOKEN.
	AnthropicAuth string `yaml:"anthropicAuth,omitempty" mapstructure:"anthropicAuth,omitempty"`
	// OAuthProvider selects an embedded CLIProxyAPI OAuth backend. Supported
	// values are chatgpt and gemini. The legacy codex value remains readable.
	OAuthProvider string `yaml:"oauthProvider,omitempty" mapstructure:"oauthProvider,omitempty"`

	// Custom model configuration (Claude Code native features)
	CustomModelID  string            `yaml:"customModelId,omitempty" mapstructure:"customModelId,omitempty"`   // ANTHROPIC_CUSTOM_MODEL_OPTION
	OpusModel      string            `yaml:"opusModel,omitempty" mapstructure:"opusModel,omitempty"`           // ANTHROPIC_DEFAULT_OPUS_MODEL
	SonnetModel    string            `yaml:"sonnetModel,omitempty" mapstructure:"sonnetModel,omitempty"`       // ANTHROPIC_DEFAULT_SONNET_MODEL
	HaikuModel     string            `yaml:"haikuModel,omitempty" mapstructure:"haikuModel,omitempty"`         // ANTHROPIC_DEFAULT_HAIKU_MODEL
	SubagentModel  string            `yaml:"subagentModel,omitempty" mapstructure:"subagentModel,omitempty"`   // CLAUDE_CODE_SUBAGENT_MODEL
	ModelOverrides map[string]string `yaml:"modelOverrides,omitempty" mapstructure:"modelOverrides,omitempty"` // modelOverrides in settings.json
	EffortLevel    string            `yaml:"effortLevel,omitempty" mapstructure:"effortLevel,omitempty"`       // CLAUDE_CODE_EFFORT_LEVEL; empty means Default/follow Claude
}

type Config struct {
	ActiveProvider string              `yaml:"active_provider" mapstructure:"active_provider"`
	Lang           string              `yaml:"lang,omitempty" mapstructure:"lang,omitempty"`
	Providers      map[string]Provider `yaml:"providers" mapstructure:"providers"`
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
	for _, model := range strings.Split(p.Model, ",") {
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
