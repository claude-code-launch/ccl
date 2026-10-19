package claude

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// providerModelPicker builds the /model lineup for a provider: the tier
// aliases (labeled with the model each one maps to), the custom model, then
// the model pool. It replaces Claude's built-in rows, which name models a
// gateway does not serve. Each model is listed once. Catalog rate metadata
// becomes a cost marker on the row, so a model's credit multiplier is visible
// where the model is chosen. nil when there is nothing to list.
func providerModelPicker(p provider.Provider, names map[string]string, details map[string]protocol.ModelInfo) *modelPickerConfig {
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
			Label:       tier.label + " · " + catalogModelDisplayName(model, names) + modelRateBadge(model, details),
			Description: fmt.Sprintf("%s tier → %s", tier.label, request),
		})
	}
	seen := make(map[string]bool)
	if custom != "" {
		options = append(options, modelPickerOption{
			Model:       custom,
			Label:       "Custom · " + catalogModelDisplayName(p.CustomModelID, names) + modelRateBadge(p.CustomModelID, details),
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
			Label: catalogModelDisplayName(model, names) + modelRateBadge(model, details),
		})
	}
	if len(options) == 0 {
		return nil
	}
	return &modelPickerConfig{Options: options, ReplaceBuiltInOptions: true}
}

// modelRateBadge renders a catalog rate multiplier as the same cost marker the
// model listings use, so the picker and `ccl provider models` agree. A zero
// multiplier is a free model, which is worth naming rather than printing "0x".
func modelRateBadge(model string, details map[string]protocol.ModelInfo) string {
	if len(details) == 0 {
		return ""
	}
	info, ok := details[strings.ToLower(provider.StripContextSuffix(strings.TrimSpace(model)))]
	if !ok || info.RateMultiplier == nil {
		return ""
	}
	if *info.RateMultiplier == 0 {
		return " · free"
	}
	return " · " + strconv.FormatFloat(*info.RateMultiplier, 'f', -1, 64) + "x"
}
