package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AnthropicModelResponse is the Anthropic-compatible /v1/models list payload.
type AnthropicModelResponse struct {
	Data    []AnthropicModel `json:"data"`
	FirstId string           `json:"firstId"`
	HasMore bool             `json:"hasMore"`
	LastId  string           `json:"lastId"`
}

type AnthropicModel struct {
	CreatedAt          time.Time `json:"created_at"`
	DisplayName        string    `json:"display_name"`
	ID                 string    `json:"id"`
	Type               string    `json:"type"`
	MaxInputTokens     int       `json:"max_input_tokens,omitempty"`
	MaxOutputTokens    int       `json:"max_output_tokens,omitempty"`
	RateMultiplier     *float64  `json:"rate_multiplier,omitempty"`
	RateUnit           string    `json:"rate_unit,omitempty"`
	IsNew              bool      `json:"is_new,omitempty"`
	PromotionAvailable bool      `json:"promotion_available,omitempty"`
}

func GetAnthropicModels(baseURL, key string) (string, error) {
	return GetAnthropicModelsWithAuth(baseURL, key, "x-api-key")
}

// GetAnthropicModelsWithAuth fetches Anthropic-compatible models using either
// the official x-api-key header or a Bearer token used by some routers.
func GetAnthropicModelsWithAuth(baseURL, key, authStyle string) (string, error) {
	infos, err := GetAnthropicModelInfosWithAuth(baseURL, key, authStyle)
	if err != nil {
		return "", err
	}
	models := make([]string, 0, len(infos))
	for _, info := range infos {
		models = append(models, info.ID)
	}
	return strings.Join(models, ","), nil
}

// GetAnthropicModelInfosWithAuth fetches model IDs and optional display,
// token-limit, rate, and catalog badge metadata from an Anthropic-compatible
// /v1/models endpoint. Unknown extension fields remain safely ignored.
func GetAnthropicModelInfosWithAuth(baseURL, key, authStyle string) ([]ModelInfo, error) {
	if key == "" {
		return nil, errors.New("api key is empty")
	}
	modelsURL := NormalizeAnthropicModelsURL(baseURL)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		modelsURL,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}

	if strings.EqualFold(authStyle, "bearer") {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		req.Header.Set("x-api-key", key)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}

	var result AnthropicModelResponse
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, ModelInfo{
				ID:                 m.ID,
				DisplayName:        m.DisplayName,
				ContextWindow:      m.MaxInputTokens,
				MaxOutputTokens:    m.MaxOutputTokens,
				RateMultiplier:     m.RateMultiplier,
				RateUnit:           m.RateUnit,
				IsNew:              m.IsNew,
				PromotionAvailable: m.PromotionAvailable,
			})
		}
	}

	return models, nil
}
