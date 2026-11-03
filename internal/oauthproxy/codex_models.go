package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/claude-code-launch/ccl/internal/codexidentity"
	"github.com/claude-code-launch/ccl/internal/protocol"
)

func discoverCodexModels(ctx context.Context, endpoint string, authorizer *codexOAuthAuthorizer) ([]string, error) {
	models, err := discoverCodexModelInfos(ctx, endpoint, authorizer)
	return codexModelIDs(models), err
}

func discoverCodexModelInfos(ctx context.Context, endpoint string, authorizer *codexOAuthAuthorizer) ([]protocol.ModelInfo, error) {
	auth, err := authorizer.authorize(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("discover GPT models: %w", err)
	}
	models, status, err := requestCodexModelInfos(ctx, authorizer.client, endpoint, auth)
	if status == http.StatusUnauthorized {
		auth, refreshErr := authorizer.authorize(withRejectedOAuthToken(ctx, auth.token), true)
		if refreshErr != nil {
			return nil, fmt.Errorf("discover GPT models: refresh rejected credential: %w", refreshErr)
		}
		models, _, err = requestCodexModelInfos(ctx, authorizer.client, endpoint, auth)
	}
	return models, err
}

func requestCodexModels(ctx context.Context, client *http.Client, endpoint string, auth codexResponsesAuthorization) ([]string, int, error) {
	models, status, err := requestCodexModelInfos(ctx, client, endpoint, auth)
	return codexModelIDs(models), status, err
}

func requestCodexModelInfos(ctx context.Context, client *http.Client, endpoint string, auth codexResponsesAuthorization) ([]protocol.ModelInfo, int, error) {
	target := strings.TrimRight(normalizeOpenAIBaseURL(endpoint), "/") + "/models?" + url.Values{
		"client_version": {codexidentity.ClientVersion},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("discover GPT models: create request: %w", err)
	}
	codexidentity.ApplyClientHeaders(request.Header)
	request.Header.Set("Authorization", "Bearer "+auth.token)
	request.Header.Set("Accept", "application/json")
	if auth.accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", auth.accountID)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("discover GPT models: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("discover GPT models: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, fmt.Errorf("discover GPT models: HTTP %d", response.StatusCode)
	}
	var catalog struct {
		Models []struct {
			Slug                  string                    `json:"slug"`
			Visibility            string                    `json:"visibility"`
			DisplayName           string                    `json:"display_name"`
			ContextWindow         int                       `json:"context_window"`
			MaxContextWindow      int                       `json:"max_context_window"`
			InputModalities       []string                  `json:"input_modalities"`
			ReasoningLevels       []protocol.ReasoningLevel `json:"supported_reasoning_levels"`
			DefaultReasoningLevel string                    `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, response.StatusCode, fmt.Errorf("decode GPT model catalog: %w", err)
	}
	models := make([]protocol.ModelInfo, 0, len(catalog.Models))
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		id := strings.TrimSpace(model.Slug)
		visibility := strings.ToLower(strings.TrimSpace(model.Visibility))
		if id == "" || seen[strings.ToLower(id)] || (visibility != "" && visibility != "list") {
			continue
		}
		seen[strings.ToLower(id)] = true
		window := model.ContextWindow
		if window == 0 {
			window = model.MaxContextWindow
		}
		models = append(models, protocol.ModelInfo{ID: id, DisplayName: model.DisplayName, ContextWindow: window, InputModalities: model.InputModalities, ReasoningLevels: model.ReasoningLevels, DefaultReasoningLevel: model.DefaultReasoningLevel})
	}
	if len(models) == 0 {
		return nil, response.StatusCode, fmt.Errorf("discover GPT models: response contained no visible models")
	}
	return models, response.StatusCode, nil
}

func codexModelIDs(models []protocol.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}
