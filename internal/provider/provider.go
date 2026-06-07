package provider

type Provider struct {
	Name     string `mapstructure:"name"`
	Type     string `mapstructure:"type"`
	Endpoint string `mapstructure:"endpoint"`
	APIKey   string `mapstructure:"api_key"`
	Model    string `mapstructure:"model"`
}

type Config struct {
	ActiveProvider string              `mapstructure:"active_provider"`
	Providers      map[string]Provider `mapstructure:"providers"`
}
