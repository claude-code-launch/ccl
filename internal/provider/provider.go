package provider

type Provider struct {
	Name     string            `yaml:"name" mapstructure:"name"`
	Type     string            `yaml:"type" mapstructure:"type"`
	Endpoint string            `yaml:"endpoint" mapstructure:"endpoint"`
	APIKey   string            `yaml:"apikey" mapstructure:"apikey"`
	Model    string            `yaml:"model" mapstructure:"model"`
	Env      map[string]string `yaml:"env,omitempty" mapstructure:"env,omitempty"`
}

type Config struct {
	ActiveProvider string              `yaml:"active_provider" mapstructure:"active_provider"`
	Providers      map[string]Provider `yaml:"providers" mapstructure:"providers"`
}
