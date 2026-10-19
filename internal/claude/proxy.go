package claude

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

// Proxy is a standalone runtime and the same model/settings view used by the
// launcher. Settings contain only the local credential, never the upstream key.
type Proxy struct {
	Runtime  *oauthproxy.Runtime
	Settings map[string]json.RawMessage
	session  *providersession.Session
}

func UserSettingsPath() string { return filepath.Join(claudeConfigDir(), "settings.json") }

func PrepareProxy(ctx context.Context, p provider.Provider) (*Proxy, error) {
	session, err := providersession.PrepareProxy(ctx, p)
	if err != nil {
		return nil, err
	}
	c := &providerContext{provider: session.Provider, baseURL: session.BaseURL, useProxy: true, session: session,
		modelNames: session.Runtime.ModelDisplayNames(), modelDetails: session.Runtime.ModelDetails()}
	if err := c.resolveModel(ctx); err != nil {
		session.Close()
		return nil, err
	}
	settings := c.settings()
	if settings.Model == "" {
		settings.Model = "sonnet"
	}
	// A direct Claude invocation cannot have its inherited environment cleaned
	// by CCL. Explicitly neutralize selectors and competing credentials in the
	// user settings while the standalone proxy is active.
	for _, key := range inheritedEnvToDrop(settings, true) {
		if _, exists := settings.Env[key]; exists && !strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
			continue
		}
		value := ""
		if strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
			value = "0"
		}
		settings.Env[key] = value
	}
	settings.Env["ANTHROPIC_API_KEY"] = ""
	for _, key := range provider.ManagedContextEnvKeys() {
		if _, exists := settings.Env[key]; !exists {
			settings.Env[key] = ""
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		session.Close()
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		session.Close()
		return nil, err
	}
	// Old user mappings must not route models around the active provider.
	if len(fields["modelOverrides"]) == 0 {
		fields["modelOverrides"] = json.RawMessage(`{}`)
	}
	return &Proxy{Runtime: session.Runtime, Settings: fields, session: session}, nil
}

func (p *Proxy) Close() {
	if p != nil {
		p.session.Close()
	}
}
