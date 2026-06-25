package provider

type Provider struct {
	Name     string            `yaml:"name" mapstructure:"name"`
	Type     string            `yaml:"type" mapstructure:"type"`
	Endpoint string            `yaml:"endpoint" mapstructure:"endpoint"`
	APIKey   string            `yaml:"apikey" mapstructure:"apikey"`
	Model    string            `yaml:"model" mapstructure:"model"`
	Env      map[string]string `yaml:"env,omitempty" mapstructure:"env,omitempty"`

	// Custom model configuration (Claude Code native features)
	CustomModelID  string            `yaml:"customModelId,omitempty" mapstructure:"customModelId,omitempty"`   // CLAUDE_CODE_MODEL_ID
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
