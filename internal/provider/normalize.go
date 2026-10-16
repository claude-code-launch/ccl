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
	for _, key := range ManagedContextEnvKeys() {
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

// EffectiveType is the protocol type ccl dispatches p with. For an OAuth
// subscription it follows from the backend, never from what a file says.
func EffectiveType(p Provider) string {
	if runtimeType, ok := OAuthRuntimeType(p.OAuthProvider); ok {
		return runtimeType
	}
	return p.Type
}

// NormalizeProvider derives the fields of one provider entry that are not
// persisted and reports whether a stored value had to change.
func NormalizeProvider(p *Provider) bool {
	changed := false

	// type: an OAuth backend's is derived (EffectiveType) and not persisted,
	// so filling it in is not a change; a stale persisted value is.
	if fixed, ok := OAuthRuntimeType(p.OAuthProvider); ok {
		if p.Type != "" && p.Type != fixed {
			changed = true
		}
		p.Type = fixed
	}
	if len(p.Env) == 0 && p.Env != nil {
		p.Env = nil
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
