package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAIRequest matches the Chat Completions endpoint payload format (/v1/chat/completions).
type OpenAIRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Tools       []OpenAITool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"` // "none", "auto", or OpenAIToolChoice
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"` // string or []OpenAIMessagePart
	Name             string           `json:"name,omitempty"`    // for tool result
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type OpenAIMessagePart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

type OpenAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type OpenAITool struct {
	Type     string            `json:"type"`
	Function OpenAIFunctionDef `json:"function"`
}

type OpenAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// OpenAIResponse matches the Chat Completions response.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIStreamChunk matches SSE lines returned from OpenAI stream.
type OpenAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        OpenAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type OpenAIStreamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}
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

func GetOpenAIModels(baseURL, apiKey string) (string, error) {
	url := baseURL + "/models"
	if strings.HasSuffix(baseURL, "/chat/completions") {
		// Make sure we don't end up with /v1/v1/chat/completions
		url = strings.Replace(baseURL, "/chat/completions", "/models", 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Printf(" resp: %d,err: %v\n", resp.StatusCode, err)

		if resp != nil {
			resp.Body.Close()
		}
		return "", err
	}

	var result ModelResponse
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
