package provider

type Provider struct {
	Name     string            `mapstructure:"name"`
	Type     string            `mapstructure:"type"`
	Endpoint string            `mapstructure:"endpoint"`
	APIKey   string            `mapstructure:"apikey"`
	Model    string            `mapstructure:"model"`
	Env      map[string]string `mapstructure:"env,omitempty"`
}

type Config struct {
	ActiveProvider string              `mapstructure:"active_provider"`
	Providers      map[string]Provider `mapstructure:"providers"`
}
