package claude

import (
	"slices"
	"sort"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
)

const modelCapabilitiesEnv = "CLAUDE_CODE_MODEL_CAPABILITIES"

// Supply only effort capability hints, scoped to exact request names. Unlike
// *_SUPPORTED_CAPABILITIES, this does not disable unrelated thinking/tool
// capabilities. Claude Code owns the menus and their supported vocabulary;
// minimal/ultra and holes in its base low/medium/high ladder cannot be added.
func (c *providerContext) applyModelEffortCapabilities(env map[string]string) {
	if provider.IsClaudeAccount(c.provider) {
		return
	}
	if _, explicit := env[modelCapabilitiesEnv]; explicit {
		return
	}
	index, _ := modelsdev.CachedIndex() // Optional local metadata; never fetch here.
	providerID := c.provider.ModelsDevProvider
	if providerID == "" {
		providerID = c.provider.OAuthProvider
		if provider.IsModelsDevType(c.provider.Type) {
			providerID = provider.ModelsDevCatalogID(c.provider)
		}
	}
	rules := map[string]string{}
	ambiguous := map[string]bool{}
	add := func(model, flags string) {
		model = strings.ToLower(provider.StripContextSuffix(model))
		// These characters have grammar meaning in the client's hint parser.
		// A model name must never create a wildcard or another model's rule.
		if model == "" || strings.ContainsAny(model, ";=,*\r\n") || ambiguous[model] {
			return
		}
		if previous, exists := rules[model]; exists && previous != flags {
			delete(rules, model)
			ambiguous[model] = true
			return
		}
		rules[model] = flags
	}
	models := modelrouting.SplitCSV(provider.RuntimeModelSpec(c.provider))
	for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_CUSTOM_MODEL_OPTION", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", SubagentModelEnv} {
		if model := env[key]; model != "" {
			models = append(models, model)
		}
	}
	for _, model := range models {
		base := provider.StripContextSuffix(model)
		request := provider.StripContextSuffix(catalogModelRequestName(base, c.modelNames))
		var efforts []string
		known := false
		if index != nil {
			efforts, known = index.Efforts(providerID, base, request)
		}
		if !known {
			for id, info := range c.modelDetails {
				if strings.EqualFold(provider.StripContextSuffix(id), base) && (info.ReasoningKnown || len(info.ReasoningLevels) > 0) {
					known = true
					for _, level := range info.ReasoningLevels {
						efforts = append(efforts, strings.ToLower(strings.TrimSpace(level.Effort)))
					}
					break
				}
			}
		}
		if !known {
			continue // Missing metadata leaves native model detection intact.
		}
		flags := []string{"-effort", "-xhigh_effort", "-max_effort"}
		for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
			if slices.Contains(efforts, level) {
				flags[0] = "effort"
			}
		}
		if slices.Contains(efforts, "xhigh") {
			flags[1] = "xhigh_effort"
		}
		if slices.Contains(efforts, "max") {
			flags[2] = "max_effort"
		}
		value := strings.Join(flags, ",")
		add(base, value)
		add(request, value)
		// Explicit overrides give custom IDs a native canonical identity.
		for canonical, target := range c.provider.ModelOverrides {
			if strings.EqualFold(provider.StripContextSuffix(target), base) || strings.EqualFold(provider.StripContextSuffix(target), request) {
				add(canonical, value)
			}
		}
	}
	var keys []string
	for key := range rules {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var entries []string
	for _, key := range keys {
		entries = append(entries, key+"="+rules[key])
	}
	if len(entries) > 0 {
		env[modelCapabilitiesEnv] = strings.Join(entries, ";")
	}
}
