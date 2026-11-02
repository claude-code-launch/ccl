package protocol

// ReasoningLevel uses the Codex model-catalog shape so a runtime can retain
// upstream capability metadata without inferring it from a model's name.
type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// ChatReasoningLevels are the standard Chat Completions effort controls for a
// backend that explicitly advertises supportsReasoning.
func ChatReasoningLevels() []ReasoningLevel {
	return []ReasoningLevel{{Effort: "low", Description: "Faster responses with less reasoning"},
		{Effort: "medium", Description: "Balance reasoning depth and response time"},
		{Effort: "high", Description: "More reasoning for complex tasks"}}
}
