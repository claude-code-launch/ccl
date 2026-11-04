package provider

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
)

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
	if migrateRetiredGeminiModels(p) {
		changed = true
	}
	return changed
}

// migrateRetiredGeminiModels rewrites the retired generated Gemini haiku ID.
// The live catalog does not contain it, so leaving the old default in place
// makes that one model 404. Any other value is a user choice and stays.
func migrateRetiredGeminiModels(p *Provider) bool {
	if p == nil || !strings.EqualFold(strings.TrimSpace(p.OAuthProvider), "gemini") {
		return false
	}
	_, _, _, haiku, _, ok := PreferredOAuthSlotDefaults("gemini")
	if !ok || strings.EqualFold(haiku, retiredGeminiHaikuModel) {
		return false
	}
	changed := false
	for _, slot := range []*string{&p.CustomModelID, &p.OpusModel, &p.SonnetModel, &p.HaikuModel, &p.FableModel, &p.SubagentModel} {
		if slot != nil && strings.EqualFold(stripContextSuffix(*slot), retiredGeminiHaikuModel) {
			*slot = replaceModelKeepingContext(*slot, haiku)
			changed = true
		}
	}
	if updated, ok := replaceCSVModel(p.Model, retiredGeminiHaikuModel, haiku); ok {
		p.Model = updated
		changed = true
	}
	return changed
}

func replaceCSVModel(pool, from, to string) (string, bool) {
	models := modelrouting.SplitCSV(pool)
	if len(models) == 0 {
		return pool, false
	}
	changed := false
	for i, model := range models {
		if strings.EqualFold(stripContextSuffix(model), from) {
			models[i] = replaceModelKeepingContext(model, to)
			changed = true
		}
	}
	if !changed {
		return pool, false
	}
	return strings.Join(models, ","), true
}

func replaceModelKeepingContext(model, replacement string) string {
	if strings.HasSuffix(strings.TrimSpace(model), "[1m]") {
		return replacement + "[1m]"
	}
	return replacement
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
