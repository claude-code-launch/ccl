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

// ModelInfo is one entry from an OpenAI-compatible /models response.
// ContextWindow is advisory only — third-party catalogs are often wrong or
// missing; callers must not treat a reported 1M window as a guarantee.
type ModelInfo struct {
	ID            string
	ContextWindow int
}

// ModelResponse is the OpenAI-compatible /models list payload.
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
		Id         string `json:"id"`
		Name       string `json:"name"`
		Object     string `json:"object"`
		Status     string `json:"status,omitempty"`
		Version    string `json:"version"`
		Modalities struct {
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

// GetOpenAIModels fetches the comma-separated model IDs from an OpenAI-compatible
// /models endpoint.
func GetOpenAIModels(baseURL, apiKey string) (string, error) {
	infos, err := GetOpenAIModelInfos(baseURL, apiKey)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return strings.Join(ids, ","), nil
}

// GetOpenAIModelInfos fetches model IDs and optional context_window metadata.
func GetOpenAIModelInfos(baseURL, apiKey string) ([]ModelInfo, error) {
	url := NormalizeOpenAIModelsURL(baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}

	var result ModelResponse
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	models := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.Id == "" {
			continue
		}
		models = append(models, ModelInfo{
			ID:            m.Id,
			ContextWindow: m.TokenLimits.ContextWindow,
		})
	}
	return models, nil
}

// ContextWindowSuggests1M reports whether a catalog context_window looks like a
// 1M-class window. Values are advisory suggestions only.
func ContextWindowSuggests1M(window int) bool {
	return window >= 900_000
}
