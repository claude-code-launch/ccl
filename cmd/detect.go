package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func detectProtocolAndModels(endpoint, apiKey string) (string, string) {
	endpoint = strings.TrimSuffix(endpoint, "/")

	// 优先探测 Anthropic
	if probeAnthropicProtocol(endpoint, apiKey) {
		models := fetchAnthropicModels(endpoint, apiKey)
		models = filterTextModels(models)
		return "anthropic", strings.Join(models, ",")
	}

	// 再探测 OpenAI，顺便解析模型列表
	if models, ok := probeOpenAIModels(endpoint, apiKey); ok {
		models = filterTextModels(models)
		return "openai", strings.Join(models, ",")
	}

	// 兜底 URL 启发式，模型列表为空
	if strings.Contains(endpoint, "anthropic.com") {
		return "anthropic", ""
	}
	return "openai", ""
}

// filterTextModels 过滤并只保留适合对话、代码或推理的文本模型
func filterTextModels(models []string) []string {
	var filtered []string
	excludeKeywords := []string{
		"embed", "whisper", "dall-e", "tts", "moderation", "audio",
		"speech", "rerank", "vector", "bge", "realtime", "voice",
	}

	for _, model := range models {
		modelLower := strings.ToLower(model)
		exclude := false
		for _, kw := range excludeKeywords {
			if strings.Contains(modelLower, kw) {
				exclude = true
				break
			}
		}
		if !exclude {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// normalizeBase 剥掉已知路径后缀，得到裸 base URL
func normalizeBase(endpoint string) string {
	for _, suffix := range []string{"/v1/chat/completions", "/v1/messages", "/v1"} {
		if before, found := strings.CutSuffix(endpoint, suffix); found {
			return before
		}
	}
	return endpoint
}

// probeAnthropicProtocol 向 /v1/messages POST 空 body，
// 通过响应 JSON 顶层 "type" 字段识别 Anthropic 协议。
func probeAnthropicProtocol(endpoint, apiKey string) bool {
	url := normalizeBase(endpoint) + "/v1/messages"

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(`{}`))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return false
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}

	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return false
	}

	t, _ := result["type"].(string)
	return t == "error" || t == "message"
}

// fetchAnthropicModels 用 Anthropic 认证头 GET /v1/models，
// 响应结构：{"data": [{"id": "claude-xxx", "type": "model", ...}, ...]}
func fetchAnthropicModels(endpoint, apiKey string) []string {
	url := normalizeBase(endpoint) + "/v1/models"

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	return parseModelsResponse(resp.Body)
}

// probeOpenAIModels GET /v1/models 用 Bearer 认证，200 OK 才算探测成功，
// 同时解析模型列表，协议检测与取模型合并为一次请求。
// 响应结构：{"object": "list", "data": [{"id": "gpt-4", ...}, ...]}
func probeOpenAIModels(endpoint, apiKey string) ([]string, bool) {
	url := normalizeBase(endpoint) + "/v1/models"

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, false
	}
	defer resp.Body.Close()

	// 200 OK 即确认为 OpenAI 兼容协议，模型列表尽力解析
	return parseModelsResponse(resp.Body), true
}

// parseModelsResponse 解析两种协议共同的模型列表格式
// {"data": [{"id": "xxx"}, ...]}
func parseModelsResponse(r io.Reader) []string {
	data, err := io.ReadAll(io.LimitReader(r, 32*1024))
	if err != nil {
		return nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil {
		return nil
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models
}
