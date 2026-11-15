package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/codexidentity"
)

// ModelInfo is one entry from an OpenAI-compatible /models response.
// ContextWindow is advisory only — third-party catalogs are often wrong or
// missing; callers must not treat a reported 1M window as a guarantee.
type ModelInfo struct {
	ID                    string
	DisplayName           string
	ContextWindow         int
	MaxOutputTokens       int
	InputModalities       []string
	RateMultiplier        *float64
	RateUnit              string
	IsNew                 bool
	PromotionAvailable    bool
	ReasoningLevels       []ReasoningLevel
	DefaultReasoningLevel string
	// ReasoningKnown distinguishes an authoritative empty effort list from
	// missing metadata, so compatibility defaults do not invent controls.
	ReasoningKnown bool
}

// ModelResponse is the OpenAI-compatible /models list payload.
// Context windows appear either nested under token_limits (some gateways) or
// as a top-level context_window (Codex / OpenAI-style catalogs).
type ModelResponse struct {
	Data []struct {
		Created  int    `json:"created"`
		Domain   string `json:"domain"`
		Features struct {
			StructuredOutputs struct {
				JsonObject bool `json:"json_object"`
				JsonSchema bool `json:"json_schema"`
			} `json:"structured_outputs,omitempty"`
			Tools struct {
				FunctionCalling bool `json:"function_calling"`
			} `json:"tools,omitempty"`
			Batch struct {
				BatchChat bool `json:"batch_chat"`
				BatchJob  bool `json:"batch_job"`
			} `json:"batch,omitempty"`
			Cache struct {
				PrefixCache  bool `json:"prefix_cache"`
				SessionCache bool `json:"session_cache"`
			} `json:"cache,omitempty"`
		} `json:"features"`
		Id                           string           `json:"id"`
		Name                         string           `json:"name"`
		Object                       string           `json:"object"`
		Status                       string           `json:"status,omitempty"`
		Version                      string           `json:"version"`
		ContextWindow                int              `json:"context_window,omitempty"`
		MaxInputTokens               int              `json:"max_input_tokens,omitempty"`
		MaxOutputTokens              int              `json:"max_output_tokens,omitempty"`
		InputModalities              []string         `json:"input_modalities,omitempty"`
		SupportedInputTypes          []string         `json:"supported_input_types,omitempty"`
		ReasoningLevels              []ReasoningLevel `json:"supported_reasoning_levels,omitempty"`
		DefaultReasoningLevel        string           `json:"default_reasoning_level,omitempty"`
		SupportsReasoningEffort      *bool            `json:"supports_reasoning_effort,omitempty"`
		ReasoningEffort              string           `json:"reasoning_effort,omitempty"`
		ReasoningEffortServerDefault string           `json:"reasoning_effort_server_default,omitempty"`
		ReasoningEfforts             json.RawMessage  `json:"reasoning_efforts,omitempty"`
		Capabilities                 struct {
			DefaultReasoningEffort string          `json:"default_reasoning_effort,omitempty"`
			ReasoningEffort        json.RawMessage `json:"reasoning_effort,omitempty"`
		} `json:"capabilities,omitempty"`
		// Optional gateway and CCL model metadata.
		RateMultiplier     *float64 `json:"rate_multiplier,omitempty"`
		RateUnit           string   `json:"rate_unit,omitempty"`
		IsNew              bool     `json:"is_new,omitempty"`
		PromotionAvailable bool     `json:"promotion_available,omitempty"`
		Modalities         struct {
			Input            []string `json:"input,omitempty"`
			Output           []string `json:"output,omitempty"`
			InputModalities  []string `json:"input_modalities,omitempty"`
			OutputModalities []string `json:"output_modalities,omitempty"`
		} `json:"modalities,omitempty"`
		TaskType    []string `json:"task_type,omitempty"`
		TokenLimits struct {
			ContextWindow           int `json:"context_window,omitempty"`
			MaxInputTokenLength     int `json:"max_input_token_length,omitempty"`
			MaxOutputTokenLength    int `json:"max_output_token_length,omitempty"`
			MaxReasoningTokenLength int `json:"max_reasoning_token_length,omitempty"`
		} `json:"token_limits,omitempty"`
	} `json:"data"`
	Object string `json:"object"`
}

// GetOpenAIModelsContext is GetOpenAIModels with caller-controlled cancellation.
func GetOpenAIModelsContext(ctx context.Context, baseURL, apiKey string) (string, error) {
	infos, err := GetOpenAIModelInfosContext(ctx, baseURL, apiKey)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return strings.Join(ids, ","), nil
}

// fetchModelsPayloadWithHeaders performs an authenticated GET against a models
// endpoint and decodes the JSON body into out.
func fetchModelsPayloadWithHeaders(url, apiKey string, headers http.Header, out any) error {
	return fetchModelsPayloadWithContext(context.Background(), url, apiKey, headers, out)
}

func fetchModelsPayloadWithContext(ctx context.Context, url, apiKey string, headers http.Header, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode models response: %w", err)
	}
	return nil
}

// GetOpenAIModelInfos fetches model IDs and optional context_window metadata.
func GetOpenAIModelInfos(baseURL, apiKey string) ([]ModelInfo, error) {
	return GetOpenAIModelInfosContext(context.Background(), baseURL, apiKey)
}

// GetOpenAIModelInfosContext is GetOpenAIModelInfos with caller-controlled cancellation.
func GetOpenAIModelInfosContext(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	var result ModelResponse
	if err := fetchModelsPayloadWithContext(ctx, NormalizeOpenAIModelsURL(baseURL), apiKey, nil, &result); err != nil {
		return nil, err
	}
	models := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.Id == "" {
			continue
		}
		// Prefer nested token_limits when present; otherwise accept top-level.
		window := m.TokenLimits.ContextWindow
		if window == 0 {
			window = m.ContextWindow
		}
		if window == 0 {
			window = m.MaxInputTokens
		}
		maxOutput := m.TokenLimits.MaxOutputTokenLength
		if maxOutput == 0 {
			maxOutput = m.MaxOutputTokens
		}
		levels, defaultLevel, known := m.ReasoningLevels, strings.TrimSpace(m.DefaultReasoningLevel), len(m.ReasoningLevels) > 0
		if !known {
			levels, defaultLevel, known = ParseRemoteReasoningEfforts(
				m.SupportsReasoningEffort,
				m.ReasoningEfforts,
				m.ReasoningEffortServerDefault,
				m.ReasoningEffort,
				m.Capabilities.DefaultReasoningEffort,
				m.Capabilities.ReasoningEffort,
			)
		} else if m.SupportsReasoningEffort != nil && !*m.SupportsReasoningEffort {
			levels, defaultLevel, known = nil, "", true
		}
		models = append(models, ModelInfo{
			ID:                    m.Id,
			DisplayName:           strings.TrimSpace(m.Name),
			ContextWindow:         window,
			MaxOutputTokens:       maxOutput,
			InputModalities:       modelInputModalities(m.InputModalities, m.SupportedInputTypes, m.Modalities.InputModalities, m.Modalities.Input),
			RateMultiplier:        m.RateMultiplier,
			RateUnit:              strings.TrimSpace(m.RateUnit),
			IsNew:                 m.IsNew,
			PromotionAvailable:    m.PromotionAvailable,
			ReasoningLevels:       levels,
			DefaultReasoningLevel: defaultLevel,
			ReasoningKnown:        known,
		})
	}
	return models, nil
}

// ParseRemoteReasoningEfforts decodes Grok/xAI-style reasoning metadata from
// GET /v1/models (supports_reasoning_effort, reasoning_efforts as string or
// {value,label,description,default} entries, reasoning_effort_server_default,
// and capabilities.reasoning_effort).
func ParseRemoteReasoningEfforts(
	supports *bool,
	rawEfforts json.RawMessage,
	serverDefault, legacyDefault, capDefault string,
	capEfforts json.RawMessage,
) ([]ReasoningLevel, string, bool) {
	if supports != nil && !*supports {
		return nil, "", true
	}
	levels, optionDefault, hasList := decodeRawEffortList(rawEfforts)
	if !hasList {
		var explicitFalse bool
		levels, optionDefault, hasList, explicitFalse = decodeCapabilityEfforts(capEfforts)
		if explicitFalse {
			return nil, "", true
		}
	}
	if !hasList {
		if supports != nil && *supports {
			levels = ChatReasoningLevels()
			chosen := firstNonEmpty(serverDefault, capDefault, legacyDefault, "medium")
			return levels, strings.ToLower(chosen), true
		}
		return nil, "", false
	}
	if len(levels) == 0 {
		return nil, "", true
	}
	chosen := strings.ToLower(firstNonEmpty(serverDefault, capDefault, optionDefault, legacyDefault))
	if chosen != "" {
		matched := false
		for _, level := range levels {
			if strings.EqualFold(level.Effort, chosen) {
				chosen = level.Effort
				matched = true
				break
			}
		}
		if !matched {
			chosen = ""
		}
	}
	if chosen == "" {
		chosen = levels[0].Effort
	}
	return levels, chosen, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func decodeRawEffortList(raw json.RawMessage) ([]ReasoningLevel, string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, "", false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, "", false
	}
	levels := make([]ReasoningLevel, 0, len(items))
	seen := make(map[string]bool, len(items))
	defaultEffort := ""
	for _, item := range items {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			effort := strings.ToLower(strings.TrimSpace(s))
			if effort == "" || effort == "none" || seen[effort] {
				continue
			}
			seen[effort] = true
			levels = append(levels, ReasoningLevel{Effort: effort, Description: defaultEffortDescription(effort)})
			continue
		}
		var obj struct {
			Value       string `json:"value"`
			Effort      string `json:"effort"`
			Label       string `json:"label"`
			Description string `json:"description"`
			Default     bool   `json:"default"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		effort := strings.ToLower(firstNonEmpty(obj.Value, obj.Effort))
		if effort == "" || effort == "none" || seen[effort] {
			continue
		}
		seen[effort] = true
		desc := firstNonEmpty(obj.Description, obj.Label, defaultEffortDescription(effort))
		levels = append(levels, ReasoningLevel{Effort: effort, Description: desc})
		if obj.Default && defaultEffort == "" {
			defaultEffort = effort
		}
	}
	return levels, defaultEffort, true
}

func decodeCapabilityEfforts(raw json.RawMessage) ([]ReasoningLevel, string, bool, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, "", false, false
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		if !flag {
			return nil, "", false, true
		}
		return ChatReasoningLevels(), "medium", true, false
	}
	if levels, def, ok := decodeRawEffortList(raw); ok {
		return levels, def, true, false
	}
	var wrapped struct {
		Efforts json.RawMessage `json:"efforts"`
		Values  json.RawMessage `json:"values"`
		Default string          `json:"default"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		if levels, def, ok := decodeRawEffortList(wrapped.Efforts); ok {
			return levels, firstNonEmpty(def, wrapped.Default), true, false
		}
		if levels, def, ok := decodeRawEffortList(wrapped.Values); ok {
			return levels, firstNonEmpty(def, wrapped.Default), true, false
		}
	}
	return nil, "", false, false
}

func defaultEffortDescription(effort string) string {
	switch effort {
	case "minimal":
		return "Minimal reasoning for fast responses"
	case "low":
		return "Fast responses with lighter reasoning"
	case "medium":
		return "Balances speed and reasoning depth for everyday tasks"
	case "high":
		return "Greater reasoning depth for complex problems"
	case "xhigh":
		return "Extra high reasoning depth for complex problems"
	case "max":
		return "Maximum reasoning depth for the hardest problems"
	case "ultra":
		return "Maximum reasoning with automatic task delegation"
	default:
		return effort
	}
}

func modelInputModalities(candidates ...[]string) []string {
	for _, modalities := range candidates {
		normalized := normalizeModalities(modalities)
		if len(normalized) > 0 {
			return normalized
		}
	}
	return nil
}

func normalizeModalities(modalities []string) []string {
	if len(modalities) == 0 {
		return nil
	}
	var extras []string
	seen := make(map[string]bool, len(modalities))
	for _, item := range modalities {
		m := strings.ToLower(strings.TrimSpace(item))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if m != "text" && m != "image" {
			extras = append(extras, m)
		}
	}
	out := make([]string, 0, len(seen))
	if seen["text"] {
		out = append(out, "text")
	}
	if seen["image"] {
		out = append(out, "image")
	}
	out = append(out, extras...)
	return out
}

// codexClientVersionQuery selects the Codex client catalog, which is the only
// shape some subscription and compatible gateways expose with context windows.
const codexClientVersionQuery = codexidentity.ClientVersion

// codexClientModelsResponse is the Codex client catalog payload. Entries reuse
// the Codex model template, where the id lives in "slug".
type codexClientModelsResponse struct {
	Models []struct {
		ID                    string           `json:"id"`
		Slug                  string           `json:"slug"`
		ContextWindow         int              `json:"context_window,omitempty"`
		MaxContextWindow      int              `json:"max_context_window,omitempty"`
		MaxOutputTokens       int              `json:"max_output_tokens,omitempty"`
		InputModalities       []string         `json:"input_modalities,omitempty"`
		ReasoningLevels       []ReasoningLevel `json:"supported_reasoning_levels,omitempty"`
		DefaultReasoningLevel string           `json:"default_reasoning_level,omitempty"`
	} `json:"models"`
}

// GetCodexClientModelInfos reads the Codex-flavoured model catalog exposed by
// the upstream /v1/models?client_version=… endpoint.
//
// This matters for subscription (OAuth) providers: the plain OpenAI list is
// trimmed to id/object/created/owned_by, so it never reveals a context window,
// while the Codex catalog reports the window the backend advertises for the
// account. Values remain advisory — the server may still enforce less.
func GetCodexClientModelInfos(baseURL, apiKey string) ([]ModelInfo, error) {
	url := NormalizeOpenAIModelsURL(baseURL)
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	var payload codexClientModelsResponse
	headers := make(http.Header)
	codexidentity.ApplyClientHeaders(headers)
	if err := fetchModelsPayloadWithHeaders(url+separator+"client_version="+codexClientVersionQuery, apiKey, headers, &payload); err != nil {
		return nil, err
	}
	models := make([]ModelInfo, 0, len(payload.Models))
	for _, model := range payload.Models {
		id := strings.TrimSpace(model.Slug)
		if id == "" {
			id = strings.TrimSpace(model.ID)
		}
		if id == "" {
			continue
		}
		window := model.ContextWindow
		if window == 0 {
			window = model.MaxContextWindow
		}
		models = append(models, ModelInfo{
			ID:                    id,
			ContextWindow:         window,
			MaxOutputTokens:       model.MaxOutputTokens,
			InputModalities:       normalizeModalities(model.InputModalities),
			ReasoningLevels:       model.ReasoningLevels,
			DefaultReasoningLevel: model.DefaultReasoningLevel,
		})
	}
	return models, nil
}

// ContextWindowSuggests1M reports whether a catalog context_window looks like a
// 1M-class window. Values are advisory suggestions only.
func ContextWindowSuggests1M(window int) bool {
	return window >= 900_000
}
