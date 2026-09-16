package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAutoClawLiveCompatibility is intentionally opt-in because it consumes
// managed-plan quota. Pass a credential basename, never a token:
//
//	CCL_AUTOCLAW_LIVE_CREDENTIAL=autoclaw-....json go test ./internal/oauthproxy -run TestAutoClawLiveCompatibility -v
//
// The regular unit tests cover the exact converted request shape without
// network access. This check catches drift in AutoClaw's private managed API.
func TestAutoClawLiveCompatibility(t *testing.T) {
	credentialFile := strings.TrimSpace(os.Getenv("CCL_AUTOCLAW_LIVE_CREDENTIAL"))
	if credentialFile == "" {
		t.Skip("set CCL_AUTOCLAW_LIVE_CREDENTIAL to run the quota-consuming AutoClaw integration test")
	}
	if filepath.Base(credentialFile) != credentialFile {
		t.Fatal("CCL_AUTOCLAW_LIVE_CREDENTIAL must be a credential basename, not a path")
	}

	contract, _ := autoClawEffectiveContract()
	modelSpec := strings.Join(autoClawCatalogIDs(contract.models), ",")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runtime, err := startAutoClawOAuth(ctx, "", modelSpec, credentialFile)
	if err != nil {
		t.Fatalf("start AutoClaw runtime: %v", err)
	}
	t.Cleanup(runtime.Stop)

	toolModel := autoClawPreferredImageModel(contract)
	if toolModel == "" && len(contract.models) > 0 {
		toolModel = contract.models[0].id
	}
	if toolModel == "" {
		t.Fatal("AutoClaw catalog is empty")
	}

	t.Run("tool-schema", func(t *testing.T) {
		response := postAutoClawLiveRequest(t, ctx, runtime, map[string]any{
			"model": toolModel, "max_tokens": 32, "stream": true,
			"tools": []any{map[string]any{
				"name": "echo", "description": "Return the supplied text.",
				"input_schema": map[string]any{
					"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required": []string{"text"},
				},
			}},
			"tool_choice": map[string]any{"type": "tool", "name": "echo"},
			"messages":    []any{map[string]any{"role": "user", "content": "Call echo with text OK."}},
		})
		if !strings.Contains(response, `"type":"message_stop"`) {
			t.Fatalf("AutoClaw tool response did not finish: %s", truncateAutoClawLiveResponse(response))
		}
	})

	reasoningModel := ""
	for _, model := range contract.models {
		if model.requiresAssistantReasoningContent {
			reasoningModel = model.id
			break
		}
	}
	if reasoningModel != "" {
		t.Run("assistant-history", func(t *testing.T) {
			response := postAutoClawLiveRequest(t, ctx, runtime, map[string]any{
				"model": reasoningModel, "max_tokens": 8, "stream": true,
				"messages": []any{
					map[string]any{"role": "user", "content": "Reply A."},
					map[string]any{"role": "assistant", "content": "A"},
					map[string]any{"role": "user", "content": "Reply OK only."},
				},
			})
			if !strings.Contains(response, `"type":"message_stop"`) {
				t.Fatalf("AutoClaw history response did not finish: %s", truncateAutoClawLiveResponse(response))
			}
		})
	}

	nonImageModel := ""
	for _, model := range contract.models {
		if !autoClawModelSupportsImage(contract.models, model.id) {
			nonImageModel = model.id
			break
		}
	}
	if nonImageModel != "" && autoClawPreferredImageModel(contract) != "" {
		t.Run("image-reroute", func(t *testing.T) {
			response := postAutoClawLiveRequest(t, ctx, runtime, map[string]any{
				"model": nonImageModel, "max_tokens": 8, "stream": true,
				"messages": []any{map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "image", "source": map[string]any{
							"type": "base64", "media_type": "image/png",
							"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
						}},
						map[string]any{"type": "text", "text": "Reply OK only."},
					},
				}},
			})
			if !strings.Contains(response, `"type":"message_stop"`) {
				t.Fatalf("AutoClaw image response did not finish: %s", truncateAutoClawLiveResponse(response))
			}
		})
	}
}

func postAutoClawLiveRequest(t *testing.T, ctx context.Context, runtime *Runtime, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runtime.Endpoint()+"/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("AutoClaw live request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("AutoClaw live status=%d: %s", response.StatusCode, truncateAutoClawLiveResponse(string(body)))
	}
	return string(body)
}

func truncateAutoClawLiveResponse(value string) string {
	const limit = 2048
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
