package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicRequest matches the structure sent by Claude Code to /v1/messages.
type AnthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        any                  `json:"system,omitempty"` // Can be string or []ContentBlock
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinking   `json:"thinking,omitempty"`
}

type AnthropicThinking struct {
	Type         string `json:"type"` // e.g. "enabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // Can be string or []ContentBlock
}

type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // for tool_use
	Name  string          `json:"name,omitempty"`  // for tool_use
	Input json.RawMessage `json:"input,omitempty"` // for tool_use, can be arbitrary object

	Thinking  string `json:"thinking,omitempty"`  // for thinking
	Signature string `json:"signature,omitempty"` // for thinking_delta

	ToolUseID string `json:"tool_use_id,omitempty"` // for tool_result
	Content   any    `json:"content,omitempty"`     // for tool_result, can be string or []ContentBlock
	IsError   bool   `json:"is_error,omitempty"`    // for tool_result
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema
}

type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// AnthropicResponse matches the structure expected by Claude Code.
type AnthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        AnthropicUsage `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
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

// GetAnthropicModels 通过 API Key 和 BaseURL 获取 Anthropic 模型列表
func GetAnthropicModels(baseURL, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("api key 不能为空")
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		baseURL+"/v1/models",
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("Failed to get Anthropic models: %s\n", err.Error())
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {

		errmsg, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Failed to get Anthropic models: %s\n", err.Error())
			return "", err
		}
		fmt.Printf("[Anthropic config error] url:%s,key:%s, status:%d, msg:%s \n", baseURL, key, resp.StatusCode, string(errmsg))
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
