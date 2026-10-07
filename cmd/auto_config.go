package cmd

import (
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/slotrec"
)

// AutoRecommendation is the slot recommendation the config page applies.
type AutoRecommendation = slotrec.Recommendation

// RecommendModels is slotrec.Recommend, the one slot recommender ccl uses.
func RecommendModels(current provider.Provider, models []string, metadata map[string]protocol.ModelInfo) AutoRecommendation {
	return slotrec.Recommend(current, models, metadata)
}

func recommendedOneMModel(model string) bool {
	return slotrec.RecommendedOneMModel(model)
}
