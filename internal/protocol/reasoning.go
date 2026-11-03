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

// GPTDefaultReasoningLevel is the picker default on GPT flagship models such
// as gpt-6.1-sol. Callers that invent a ladder use this instead of medium.
const GPTDefaultReasoningLevel = "low"

// GPTReasoningLevels is the six-rung effort ladder GPT flagship models
// advertise, in desktop slider order. Descriptions match that catalog.
// Each call returns a new slice.
func GPTReasoningLevels() []ReasoningLevel {
	return []ReasoningLevel{
		{Effort: "low", Description: "Fast responses with lighter reasoning"},
		{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks"},
		{Effort: "high", Description: "Greater reasoning depth for complex problems"},
		{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems"},
		{Effort: "max", Description: "Maximum reasoning depth for the hardest problems"},
		{Effort: "ultra", Description: "Maximum reasoning with automatic task delegation"},
	}
}
