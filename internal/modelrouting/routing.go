package modelrouting

import "strings"

const (
	TierOpus   = "opus"
	TierSonnet = "sonnet"
	TierHaiku  = "haiku"
)

func SplitCSV(models string) []string {
	var out []string
	for model := range strings.SplitSeq(models, ",") {
		model = strings.TrimSpace(model)
		if model != "" {
			out = append(out, model)
		}
	}
	return out
}

// MapModel picks a concrete upstream model for a Claude-style request.
//
// Precedence:
//  1. A single configuredModel overrides mapping entirely.
//  2. A comma-separated configuredModel is treated as the exclusive candidate pool.
//  3. Otherwise availableModels is the candidate pool.
//
// Matching order within the pool: exact (case-insensitive) name, then tier
// heuristics, then the first pool entry. An empty pool returns "" — callers must
// not invent a vendor-specific default such as deepseek-chat.
func MapModel(requestedModel string, configuredModel string, availableModels []string) string {
	if configuredModel != "" && !strings.Contains(configuredModel, ",") {
		return configuredModel
	}

	var modelPool []string
	if configuredModel != "" && strings.Contains(configuredModel, ",") {
		modelPool = SplitCSV(configuredModel)
	} else {
		for _, model := range availableModels {
			model = strings.TrimSpace(model)
			if model != "" {
				modelPool = append(modelPool, model)
			}
		}
	}

	for _, model := range modelPool {
		if strings.EqualFold(model, requestedModel) {
			return model
		}
	}

	if selected := selectModelForTier(requestedModel, modelPool); selected != "" {
		return selected
	}
	if len(modelPool) > 0 {
		return modelPool[0]
	}
	return ""
}

func requestedTier(model string) string {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "opus"):
		return TierOpus
	case strings.Contains(model, "haiku"):
		return TierHaiku
	default:
		return TierSonnet
	}
}

func selectModelForTier(requested string, models []string) string {
	tier := requestedTier(requested)
	bestModel := ""
	bestScore := 0
	for _, model := range models {
		if score := scoreModelForTier(model, tier); score > bestScore {
			bestModel = model
			bestScore = score
		}
	}
	return bestModel
}

func scoreModelForTier(model string, tier string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}

	switch tier {
	case TierOpus:
		switch {
		case strings.Contains(m, "opus"):
			return 100
		case strings.Contains(m, "deepseek-reasoner"):
			return 95
		case m == "o1" || strings.HasPrefix(m, "o1-") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
			return 90
		case strings.Contains(m, "reasoner") || strings.Contains(m, "reasoning") || strings.Contains(m, "thinking"):
			return 85
		case strings.Contains(m, "pro") || strings.Contains(m, "max") || strings.Contains(m, "ultra"):
			return 60
		}
	case TierSonnet:
		switch {
		case strings.Contains(m, "sonnet"):
			return 100
		case m == "gpt-4o" || strings.HasPrefix(m, "gpt-4o-") && !strings.Contains(m, "mini"):
			return 95
		case strings.Contains(m, "qwen") && strings.Contains(m, "plus"):
			return 90
		case strings.Contains(m, "deepseek-chat"):
			return 85
		case strings.Contains(m, "chat") || strings.Contains(m, "plus") || strings.Contains(m, "turbo") || strings.Contains(m, "fast"):
			return 55
		}
	case TierHaiku:
		switch {
		case strings.Contains(m, "haiku"):
			return 100
		case m == "gpt-4o-mini" || strings.HasPrefix(m, "gpt-4o-mini-"):
			return 95
		case strings.Contains(m, "mini") || strings.Contains(m, "nano") ||
			strings.Contains(m, "air") || strings.Contains(m, "lite") || strings.Contains(m, "flash"):
			return 60
		}
	}

	return 0
}
