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
	Data []struct {
		CreatedAt   time.Time `json:"created_at"`
		DisplayName string    `json:"display_name"`
		Id          string    `json:"id"`
		Type        string    `json:"type"`
	} `json:"data"`
	FirstId string `json:"firstId"`
	HasMore bool   `json:"hasMore"`
	LastId  string `json:"lastId"`
}

func GetAnthropicModels(baseURL, key string) (string, error) {
	return GetAnthropicModelsWithAuth(baseURL, key, "x-api-key")
}

// GetAnthropicModelsWithAuth fetches Anthropic-compatible models using either
// the official x-api-key header or a Bearer token used by some routers.
func GetAnthropicModelsWithAuth(baseURL, key, authStyle string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("api key 不能为空")
	}
	modelsURL := NormalizeAnthropicModelsURL(baseURL)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		modelsURL,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
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
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	var result AnthropicModelResponse
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.Id != "" {
			models = append(models, m.Id)
		}
	}

	return strings.Join(models, ","), nil
}
