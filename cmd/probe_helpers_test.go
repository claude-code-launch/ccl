package cmd

import (
	"context"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

// Test-only shorthands over the live probe and layout helpers.

func testSingleModelForProtocolContext(ctx context.Context, model, endpoint, apiKey, wireProtocol, anthropicAuth string, timeout time.Duration) bool {
	status, err := probeSingleModelForProtocolStatusContext(ctx, model, endpoint, apiKey, wireProtocol, anthropicAuth, timeout)
	return err == nil && status >= 200 && status < 300
}

func testSingleOpenAIModelContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	status, err := probeSingleOpenAIModelStatusContext(parent, model, endpoint, apiKey, timeout)
	return err == nil && status >= 200 && status < 300
}

func modelPoolForMapping(ctx context.Context, p provider.Provider) ([]string, error) {
	models, _, err := modelCatalogForMapping(ctx, p)
	return models, err
}

func detectProtocolAndModels(endpoint, apiKey string) (string, string, error) {
	result := detectProtocolAndModelsDetailed(endpoint, apiKey)
	return result.protocol, result.models, result.err
}

func rowAtLine(lines []string, y int) (configRowKind, bool) {
	return rowAtLineAt(lines, y, -1)
}
