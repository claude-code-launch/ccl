package provider

import "strings"

// Context preset spellings persisted in Provider.ContextPreset.
const (
	ContextPresetNameBalanced500K = "balanced-500k"
	ContextPresetNameBalanced800K = "balanced-800k"
)

// ProviderContextPreset returns p's context tier: the typed field, or — for a
// provider not yet normalized — an exact env triplet.
func ProviderContextPreset(p Provider) ContextPreset {
	switch strings.ToLower(strings.TrimSpace(p.ContextPreset)) {
	case ContextPresetNameBalanced500K:
		return ContextPresetBalanced500K
	case ContextPresetNameBalanced800K:
		return ContextPresetBalanced800K
	}
	return ContextPresetFromEnv(p.Env)
}

// SetProviderContextPreset records preset on p and removes any context env the
// field replaces.
func SetProviderContextPreset(p *Provider, preset ContextPreset) {
	if p == nil {
		return
	}
	switch preset {
	case ContextPresetBalanced500K:
		p.ContextPreset = ContextPresetNameBalanced500K
	case ContextPresetBalanced800K:
		p.ContextPreset = ContextPresetNameBalanced800K
	default:
		p.ContextPreset = ""
	}
	for _, key := range append(ManagedContextEnvKeys(), EnvContextBudgetMode) {
		delete(p.Env, key)
	}
	if len(p.Env) == 0 {
		p.Env = nil
	}
}

// ContextPresetEnv returns the Claude Code variables a provider's preset
// expands to at launch (nil for Default).
func ContextPresetEnv(p Provider) map[string]string {
	maxContext, compactWindow, compactPct, ok := ContextPresetValues(ProviderContextPreset(p))
	if !ok {
		return nil
	}
	return map[string]string{
		EnvMaxContextTokens:  maxContext,
		EnvAutoCompactWindow: compactWindow,
		EnvAutoCompactPct:    compactPct,
	}
}

// HasUnsupportedContextEnv reports hand-written context variables that match
// no preset; the launcher drops them (and says so).
func HasUnsupportedContextEnv(p Provider) bool {
	return HasManagedContextEnv(p.Env) && ContextPresetFromEnv(p.Env) == ContextPresetDefault
}

var canonicalTypes = map[string]string{
	"openai-responses":  "openai_responses",
	"responses":         "openai_responses",
	"openai(responses)": "openai_responses",
	"openai(agent)":     "openai_responses",
	"openai-chat":       "openai",
	"openai(chat)":      "openai",
}

// NormalizeProvider brings one provider entry to the current schema and reports
// whether it changed. name is its key in the providers map. legacySlots also
// moves slot values an older ccl generated as defaults; that runs once per
// config (it cannot tell a generated value from a user's identical choice).
func NormalizeProvider(p *Provider, name string, legacySlots bool) bool {
	changed := false
	set := func(field *string, value string) {
		if *field != value {
			*field = value
			changed = true
		}
	}

	// oauthProvider: infer it for configs written before it was persisted, and
	// fold the retired chatgpt/codex names into gpt.
	if p.OAuthProvider == "" {
		inferred := InferOAuthProvider(name, p.Endpoint)
		// AutoClaw has an HTTP-shaped endpoint, but its dedicated type is
		// unambiguous.
		if inferred == "" && IsAutoClawType(p.Type) {
			inferred = "autoclaw"
		}
		set(&p.OAuthProvider, inferred)
	}
	switch strings.ToLower(strings.TrimSpace(p.OAuthProvider)) {
	case "chatgpt", "codex":
		set(&p.OAuthProvider, "gpt")
	}

	// type: OAuth backends have a fixed local dispatch type; manual gateways
	// use one canonical spelling per protocol.
	if fixed, ok := OAuthRuntimeType(p.OAuthProvider); ok {
		set(&p.Type, fixed)
	} else if canonical, ok := canonicalTypes[strings.ToLower(strings.TrimSpace(p.Type))]; ok {
		set(&p.Type, canonical)
	}

	// Context: the retired ccl directive goes away; an env triplet that matches
	// a Balanced tier (including the pre-85% values) becomes the typed field.
	if _, ok := p.Env[EnvContextBudgetMode]; ok {
		delete(p.Env, EnvContextBudgetMode)
		changed = true
	}
	if p.ContextPreset == "" {
		if preset := ContextPresetFromEnv(p.Env); preset != ContextPresetDefault {
			SetProviderContextPreset(p, preset)
			changed = true
		}
	}
	if len(p.Env) == 0 && p.Env != nil {
		p.Env = nil
	}

	if legacySlots && migrateLegacyOAuthSlots(p) {
		changed = true
	}
	return changed
}

// migrateLegacyOAuthSlots replaces Grok slot values that an older ccl wrote as
// defaults with today's defaults.
func migrateLegacyOAuthSlots(p *Provider) bool {
	if !strings.EqualFold(strings.TrimSpace(p.OAuthProvider), "grok") {
		return false
	}
	custom, opus, sonnet, haiku, fable, ok := PreferredOAuthSlotDefaults(p.OAuthProvider)
	if !ok {
		return false
	}
	changed := false
	for _, slot := range []struct {
		value     *string
		legacy    string
		preferred string
	}{
		{&p.CustomModelID, "grok-4.5", custom},
		{&p.OpusModel, "grok-4.5", opus},
		{&p.FableModel, "grok-4.5", fable},
		{&p.SonnetModel, "grok-4.3", sonnet},
		{&p.HaikuModel, "grok-3-mini", haiku},
	} {
		if isPreferredDefault(*slot.value, slot.legacy) && !isPreferredDefault(*slot.value, slot.preferred) {
			*slot.value = slot.preferred
			changed = true
		}
	}
	return changed
}

// Clone returns a deep copy of p: its maps are not shared with the original.
func (p Provider) Clone() Provider {
	cloned := p
	cloned.Env = cloneStringMap(p.Env)
	cloned.ModelOverrides = cloneStringMap(p.ModelOverrides)
	cloned.ModelProtocols = cloneStringMap(p.ModelProtocols)
	return cloned
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
