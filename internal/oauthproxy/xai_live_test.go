package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestXaiGrokLiveCompatibility is opt-in because it consumes Grok subscription
// quota. Pass a credential basename, never an access token:
//
//	CCL_GROK_LIVE_CREDENTIAL=xai-....json go test ./internal/oauthproxy -run TestXaiGrokLiveCompatibility -v
//
// The test exercises the complete Claude Messages -> OpenAI Responses -> Grok
// stream path, including live catalog discovery and Grok routing headers.
func TestXaiGrokLiveCompatibility(t *testing.T) {
	credentialFile := strings.TrimSpace(os.Getenv("CCL_GROK_LIVE_CREDENTIAL"))
	if credentialFile == "" {
		t.Skip("set CCL_GROK_LIVE_CREDENTIAL to run the quota-consuming Grok integration test")
	}
	if filepath.Base(credentialFile) != credentialFile {
		t.Fatal("CCL_GROK_LIVE_CREDENTIAL must be a credential basename, not a path")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runtime, err := startXaiOAuth(ctx, "", credentialFile)
	if err != nil {
		t.Fatalf("start Grok runtime: %v", err)
	}
	t.Cleanup(runtime.Stop)
	if !slices.Contains(runtime.Models(), "grok-4.6") {
		t.Fatalf("live catalog does not contain grok-4.6: %v", runtime.Models())
	}
	if runtime.ModelCatalogIsFallback() {
		t.Fatal("live Grok catalog unexpectedly used the compatibility fallback")
	}

	requestModel := runtime.ModelDisplayNames()["grok-4.6"]
	if requestModel == "" {
		requestModel = "grok-4.6"
	}
	payload := map[string]any{
		"model": requestModel, "max_tokens": 16, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly OK."}},
	}
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
		t.Fatalf("Grok live request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Grok live status=%d: %s", response.StatusCode, truncateXaiLiveResponse(string(body)))
	}
	if !strings.Contains(string(body), `"type":"message_stop"`) {
		t.Fatalf("Grok stream did not finish: %s", truncateXaiLiveResponse(string(body)))
	}
}

func truncateXaiLiveResponse(value string) string {
	const limit = 2048
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
