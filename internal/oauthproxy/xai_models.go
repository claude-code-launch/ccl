package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
)

type xaiModel struct {
	ID                    string
	Name                  string
	ContextWindow         int
	MaxOutputTokens       int
	InputModalities       []string
	ReasoningLevels       []protocol.ReasoningLevel
	DefaultReasoningLevel string
	ReasoningKnown        bool
}

func xaiFallbackModels() []xaiModel {
	grok46Levels, _ := reasoningLevelsFromEfforts([]string{"low", "medium", "high", "xhigh"}, "high")
	grok45Levels, _ := reasoningLevelsFromEfforts([]string{"low", "medium", "high"}, "high")
	return []xaiModel{
		{
			ID:                    "grok-4.6",
			Name:                  "Grok 4.6",
			ContextWindow:         500_000,
			InputModalities:       []string{"text", "image"},
			ReasoningLevels:       grok46Levels,
			DefaultReasoningLevel: "high",
			ReasoningKnown:        true,
		},
		{
			ID:                    "grok-4.5",
			Name:                  "Grok 4.5",
			ContextWindow:         500_000,
			InputModalities:       []string{"text", "image"},
			ReasoningLevels:       grok45Levels,
			DefaultReasoningLevel: "high",
			ReasoningKnown:        true,
		},
	}
}

func discoverXaiModels(ctx context.Context, endpoint string, authorizer *xaiOAuthAuthorizer) ([]xaiModel, error) {
	if authorizer == nil {
		return nil, fmt.Errorf("discover Grok models: missing authorizer")
	}
	auth, err := authorizer.authorize(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("discover Grok models: %w", err)
	}
	models, status, err := requestXaiModels(ctx, authorizer.client, endpoint, auth)
	if status == http.StatusUnauthorized {
		refreshed, refreshErr := authorizer.authorize(withRejectedOAuthToken(ctx, auth.token), true)
		if refreshErr != nil {
			return nil, fmt.Errorf("discover Grok models: refresh rejected credential: %w", refreshErr)
		}
		models, _, err = requestXaiModels(ctx, authorizer.client, endpoint, refreshed)
	}
	return models, err
}

func requestXaiModels(ctx context.Context, client *http.Client, endpoint string, auth codexResponsesAuthorization) ([]xaiModel, int, error) {
	if client == nil {
		client = http.DefaultClient
	}
	target := strings.TrimRight(normalizeOpenAIBaseURL(endpoint), "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	applyXaiClientHeaders(request.Header, auth)
	request.Header.Set("Authorization", "Bearer "+auth.token)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("discover Grok models: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, xaiMaxErrorBytes))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("discover Grok models: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, fmt.Errorf("discover Grok models: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	DebugHTTPBody("xAI/Grok model catalog", body)
	models, err := parseXaiModels(body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(models) == 0 {
		return nil, response.StatusCode, fmt.Errorf("discover Grok models: response contained no Responses models")
	}
	return models, response.StatusCode, nil
}

func parseXaiModels(body []byte) ([]xaiModel, error) {
	var catalog struct {
		Data []struct {
			ID                           string                    `json:"id"`
			Model                        string                    `json:"model"`
			Name                         string                    `json:"name"`
			APIBackend                   string                    `json:"api_backend"`
			ContextWindow                int                       `json:"context_window"`
			MaxInputTokens               int                       `json:"max_input_tokens"`
			MaxOutputTokens              int                       `json:"max_output_tokens"`
			InputModalities              []string                  `json:"input_modalities"`
			SupportedInputTypes          []string                  `json:"supported_input_types"`
			ReasoningLevels              []protocol.ReasoningLevel `json:"supported_reasoning_levels"`
			DefaultReasoningLevel        string                    `json:"default_reasoning_level"`
			SupportsReasoningEffort      *bool                     `json:"supports_reasoning_effort"`
			ReasoningEffort              string                    `json:"reasoning_effort"`
			ReasoningEffortServerDefault string                    `json:"reasoning_effort_server_default"`
			ReasoningEfforts             json.RawMessage           `json:"reasoning_efforts"`
			Capabilities                 struct {
				DefaultReasoningEffort string          `json:"default_reasoning_effort"`
				ReasoningEffort        json.RawMessage `json:"reasoning_effort"`
			} `json:"capabilities"`
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode Grok model catalog: %w", err)
	}
	models := make([]xaiModel, 0, len(catalog.Data))
	seen := make(map[string]bool, len(catalog.Data))
	index, _ := modelsdev.CachedIndex()
	for _, entry := range catalog.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = strings.TrimSpace(entry.Model)
		}
		backend := strings.ToLower(strings.TrimSpace(entry.APIBackend))
		key := strings.ToLower(id)
		if id == "" || (backend != "" && backend != "responses") || seen[key] {
			continue
		}
		seen[key] = true
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = id
		}
		window := entry.ContextWindow
		if window == 0 {
			window = entry.MaxInputTokens
		}
		modalities := normalizeModelModalities(entry.InputModalities)
		if len(modalities) == 0 {
			modalities = normalizeModelModalities(entry.Modalities.Input)
		}
		if len(modalities) == 0 {
			modalities = normalizeModelModalities(entry.SupportedInputTypes)
		}
		if len(modalities) == 0 && index != nil {
			if resolved, ok := index.InputModalities("xai", id, name); ok {
				modalities = resolved
			}
		}
		if len(modalities) == 0 && xaiKnownVisionModel(id) {
			modalities = []string{"text", "image"}
		}
		maxOut := entry.MaxOutputTokens
		if maxOut == 0 && index != nil {
			if resolved, ok := index.MaxOutputTokens("xai", id, name); ok {
				maxOut = resolved
			}
		}
		levels, defaultLevel, known := entry.ReasoningLevels, strings.TrimSpace(entry.DefaultReasoningLevel), len(entry.ReasoningLevels) > 0
		if !known {
			levels, defaultLevel, known = protocol.ParseRemoteReasoningEfforts(
				entry.SupportsReasoningEffort,
				entry.ReasoningEfforts,
				entry.ReasoningEffortServerDefault,
				entry.ReasoningEffort,
				entry.Capabilities.DefaultReasoningEffort,
				entry.Capabilities.ReasoningEffort,
			)
		} else if entry.SupportsReasoningEffort != nil && !*entry.SupportsReasoningEffort {
			levels, defaultLevel, known = nil, "", true
		}
		if !known && index != nil {
			if efforts, found := index.Efforts("xai", id, name); found {
				levels, defaultLevel = reasoningLevelsFromEfforts(efforts, "high")
				known = true
			}
		}
		models = append(models, xaiModel{
			ID:                    id,
			Name:                  name,
			ContextWindow:         window,
			MaxOutputTokens:       maxOut,
			InputModalities:       modalities,
			ReasoningLevels:       levels,
			DefaultReasoningLevel: defaultLevel,
			ReasoningKnown:        known,
		})
	}
	return models, nil
}

func xaiKnownVisionModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	return strings.HasPrefix(lower, "grok-4") || strings.HasPrefix(lower, "grok-2-vision")
}

func xaiModelSpec(configured string, models []xaiModel) string {
	parts := make([]string, 0, 1+len(models))
	if configured = strings.TrimSpace(configured); configured != "" {
		parts = append(parts, configured)
	}
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, ",")
}

func xaiModelIDs(models []xaiModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func xaiModelDisplayNames(models []xaiModel) map[string]string {
	names := make(map[string]string, len(models))
	for _, model := range models {
		if id, name := strings.TrimSpace(model.ID), strings.TrimSpace(model.Name); id != "" && name != "" {
			names[id] = name
		}
	}
	return names
}

func addXaiDisplayAliases(service *codexResponsesService, models []xaiModel) {
	if service == nil {
		return
	}
	for _, model := range models {
		id, name := strings.TrimSpace(model.ID), strings.TrimSpace(model.Name)
		if id != "" && name != "" {
			service.modelRoute[strings.ToLower(name)] = id
		}
	}
}

func xaiModelDetails(configured string, models []xaiModel) map[string]protocol.ModelInfo {
	details := make(map[string]protocol.ModelInfo, len(models)*2)
	byID := make(map[string]xaiModel, len(models))
	build := func(alias string, model xaiModel) protocol.ModelInfo {
		levels := append([]protocol.ReasoningLevel(nil), model.ReasoningLevels...)
		return protocol.ModelInfo{
			ID:                    alias,
			DisplayName:           model.Name,
			ContextWindow:         model.ContextWindow,
			MaxOutputTokens:       model.MaxOutputTokens,
			InputModalities:       append([]string(nil), model.InputModalities...),
			ReasoningLevels:       levels,
			DefaultReasoningLevel: model.DefaultReasoningLevel,
			ReasoningKnown:        model.ReasoningKnown,
		}
	}
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		byID[strings.ToLower(id)] = model
		details[strings.ToLower(id)] = build(id, model)
		if name := strings.TrimSpace(model.Name); name != "" {
			details[strings.ToLower(name)] = build(name, model)
		}
	}
	for _, route := range runtimeModelRoutes(configured) {
		if model, ok := byID[strings.ToLower(strings.TrimSpace(route.Name))]; ok {
			alias := strings.TrimSpace(route.Alias)
			if alias != "" {
				details[strings.ToLower(alias)] = build(alias, model)
			}
		}
	}
	return details
}
