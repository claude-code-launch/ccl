package claude

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// providerModelPicker builds the /model lineup for a provider: the tier
// aliases (labeled with the model each one maps to), the custom model, then
// the model pool. It replaces Claude's built-in rows, which name models a
// gateway does not serve. Each model is listed once. nil when there is
// nothing to list.
func providerModelPicker(p provider.Provider, names map[string]string) *modelPickerConfig {
	var options []modelPickerOption
	custom := catalogModelRequestName(p.CustomModelID, names)
	// offered holds the request names a tier or the custom row already
	// reaches. The match is exact, so a pool row whose [1m] marker differs
	// stays: it opens a context window no other row does.
	offered := make(map[string]bool)
	if custom != "" {
		offered[strings.ToLower(custom)] = true
	}
	for _, tier := range []struct{ alias, label, model string }{
		{"opus", "Opus", p.OpusModel},
		{"sonnet", "Sonnet", p.SonnetModel},
		{"haiku", "Haiku", p.HaikuModel},
		{"fable", "Fable", p.FableModel},
	} {
		model := strings.TrimSpace(tier.model)
		if model == "" {
			continue
		}
		// A tier on the custom model yields to the custom row, which is the
		// one the session's settings model selects.
		request := catalogModelRequestName(model, names)
		if offered[strings.ToLower(request)] {
			continue
		}
		offered[strings.ToLower(request)] = true
		options = append(options, modelPickerOption{
			Model:       tier.alias,
			Label:       tier.label + " · " + catalogModelDisplayName(model, names),
			Description: fmt.Sprintf("%s tier → %s", tier.label, request),
		})
	}
	seen := make(map[string]bool)
	if custom != "" {
		options = append(options, modelPickerOption{
			Model:       custom,
			Label:       "Custom · " + catalogModelDisplayName(p.CustomModelID, names),
			Description: "Default model for this provider",
		})
		seen[strings.ToLower(provider.StripContextSuffix(custom))] = true
	}
	for _, model := range modelrouting.SplitCSV(p.Model) {
		request := catalogModelRequestName(model, names)
		key := strings.ToLower(provider.StripContextSuffix(request))
		if seen[key] || offered[strings.ToLower(request)] {
			continue
		}
		seen[key] = true
		options = append(options, modelPickerOption{
			Model: request,
			Label: catalogModelDisplayName(model, names),
		})
	}
	if len(options) == 0 {
		return nil
	}
	return &modelPickerConfig{Options: options, ReplaceBuiltInOptions: true}
}
