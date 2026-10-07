package claude

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// maxPickerModels caps the pool rows: a gateway can expose hundreds of models,
// and the picker is for choosing, not browsing a catalog.
const maxPickerModels = 80

// providerModelPicker builds the /model lineup for a provider: the tier
// aliases (labeled with the model each one maps to), the custom model, then
// the model pool. It replaces Claude's built-in rows, which name models a
// gateway does not serve. nil when there is nothing to list.
func providerModelPicker(p provider.Provider, names map[string]string) *modelPickerConfig {
	var options []modelPickerOption
	seen := make(map[string]bool)
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
		options = append(options, modelPickerOption{
			Model:       tier.alias,
			Label:       tier.label + " · " + catalogModelDisplayName(model, names),
			Description: fmt.Sprintf("%s tier → %s", tier.label, catalogModelRequestName(model, names)),
		})
	}
	if custom := strings.TrimSpace(p.CustomModelID); custom != "" {
		request := catalogModelRequestName(custom, names)
		options = append(options, modelPickerOption{
			Model:       request,
			Label:       "Custom · " + catalogModelDisplayName(custom, names),
			Description: "Default model for this provider",
		})
		seen[strings.ToLower(provider.StripContextSuffix(request))] = true
	}
	added := 0
	for _, model := range modelrouting.SplitCSV(p.Model) {
		request := catalogModelRequestName(model, names)
		key := strings.ToLower(provider.StripContextSuffix(request))
		if seen[key] {
			continue
		}
		if added == maxPickerModels {
			break
		}
		seen[key] = true
		added++
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
