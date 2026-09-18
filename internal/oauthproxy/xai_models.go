package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type xaiModel struct {
	ID            string
	Name          string
	ContextWindow int
}

func xaiFallbackModels() []xaiModel {
	return []xaiModel{
		{ID: "grok-4.6", Name: "Grok 4.6", ContextWindow: 500_000},
		{ID: "grok-4.5", Name: "Grok 4.5", ContextWindow: 500_000},
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
		refreshed, refreshErr := authorizer.authorize(ctx, true)
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
			ID            string `json:"id"`
			Model         string `json:"model"`
			Name          string `json:"name"`
			APIBackend    string `json:"api_backend"`
			ContextWindow int    `json:"context_window"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode Grok model catalog: %w", err)
	}
	models := make([]xaiModel, 0, len(catalog.Data))
	seen := make(map[string]bool, len(catalog.Data))
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
		models = append(models, xaiModel{ID: id, Name: name, ContextWindow: entry.ContextWindow})
	}
	return models, nil
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
