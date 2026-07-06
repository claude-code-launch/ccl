package provider

import "strings"

type Provider struct {
	Name     string            `yaml:"name" mapstructure:"name"`
	Type     string            `yaml:"type" mapstructure:"type"`
	Endpoint string            `yaml:"endpoint" mapstructure:"endpoint"`
	APIKey   string            `yaml:"apikey" mapstructure:"apikey"`
	Model    string            `yaml:"model" mapstructure:"model"`
	Env      map[string]string `yaml:"env,omitempty" mapstructure:"env,omitempty"`

	// Custom model configuration (Claude Code native features)
	CustomModelID  string            `yaml:"customModelId,omitempty" mapstructure:"customModelId,omitempty"`   // ANTHROPIC_CUSTOM_MODEL_OPTION
	OpusModel      string            `yaml:"opusModel,omitempty" mapstructure:"opusModel,omitempty"`           // ANTHROPIC_DEFAULT_OPUS_MODEL
	SonnetModel    string            `yaml:"sonnetModel,omitempty" mapstructure:"sonnetModel,omitempty"`       // ANTHROPIC_DEFAULT_SONNET_MODEL
	HaikuModel     string            `yaml:"haikuModel,omitempty" mapstructure:"haikuModel,omitempty"`         // ANTHROPIC_DEFAULT_HAIKU_MODEL
	ModelOverrides map[string]string `yaml:"modelOverrides,omitempty" mapstructure:"modelOverrides,omitempty"` // modelOverrides in settings.json
	EffortLevel    string            `yaml:"effortLevel,omitempty" mapstructure:"effortLevel,omitempty"`       // CLAUDE_CODE_EFFORT_LEVEL (low/medium/high)
	LockModel      string            `yaml:"lockModel,omitempty" mapstructure:"lockModel,omitempty"`           // model in settings.json (locks to single model)
}

type Config struct {
	ActiveProvider string              `yaml:"active_provider" mapstructure:"active_provider"`
	Lang           string              `yaml:"lang,omitempty" mapstructure:"lang,omitempty"`
	Providers      map[string]Provider `yaml:"providers" mapstructure:"providers"`
}

func IsOpenAIResponsesType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai_responses" || providerType == "openai-responses" || providerType == "responses"
}

func IsOpenAICompatibleType(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	return providerType == "openai" || IsOpenAIResponsesType(providerType)
}

// ProtocolLabel returns a short, human-friendly protocol name for display purposes
// (e.g. in the `set` TUI, `ccl list`, and `ccl doctor` output). It intentionally does
// NOT change the underlying stored provider.Type value, which remains a stable,
// machine-readable string ("anthropic", "openai", "openai_responses", ...) relied on
// throughout the codebase for dispatch logic (proxy, launcher, doctor, ...).
//
// OpenAI exposes two distinct generation protocols behind the same "openai" umbrella:
//  1. Chat Completions — the old standard, broadest compatibility: labeled "openai(chat)".
//  2. Responses — the newer, agent-oriented protocol: labeled "openai(agent)".
func ProtocolLabel(providerType string) string {
	trimmed := strings.TrimSpace(providerType)
	switch {
	case trimmed == "":
		return ""
	case IsOpenAIResponsesType(trimmed):
		return "openai(agent)"
	case strings.EqualFold(trimmed, "anthropic"):
		return "anthropic"
	default:
		return "openai(chat)"
	}
}

// NormalizeLegacyCustomSlot migrates configs created when the UI used lockModel
// as the fourth "Custom" picker slot. LockModel remains supported for manually
// authored provider configs that intentionally lock Claude Code to one model.
func NormalizeLegacyCustomSlot(p Provider) Provider {
	if strings.TrimSpace(p.CustomModelID) == "" && strings.TrimSpace(p.LockModel) != "" {
		p.CustomModelID = p.LockModel
		p.LockModel = ""
	}
	return p
}
