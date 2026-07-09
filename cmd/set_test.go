package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func assertErr(msg string) error {
	return errors.New(msg)
}

// newMockGatewayServer simulates an OpenAI-compatible gateway. Anthropic-shaped
// requests (identified by the x-api-key header) to /v1/models always fail with
// 401, simulating a non-Anthropic backend. OpenAI-shaped requests
// (Authorization: Bearer, no x-api-key) succeed with the given models.
// Requests to /v1/responses succeed only when responsesSupported is true.
func newMockGatewayServer(t *testing.T, models []string, responsesSupported bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var data []map[string]string
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !responsesSupported {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestDetectProtocolAndModelsDetectsAnthropic(t *testing.T) {
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("non-version x-api-key probe should not send bearer")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-5-sonnet","type":"model"}]}`))
	})
	mux.HandleFunc("/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/anthropic", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", proto)
	}
	if models != "claude-3-5-sonnet" {
		t.Errorf("expected models 'claude-3-5-sonnet', got %q", models)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
}

func TestDetectProtocolAndModelsClassifiesV1SuffixByShape(t *testing.T) {
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			t.Fatalf("v1 suffix should not send x-api-key")
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-6.7-flash-lite","type":"model"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL+"/v1", "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "anthropic" {
		t.Errorf("expected protocol 'anthropic', got %q", result.protocol)
	}
	if result.anthropicAuth != anthropicAuthBearer {
		t.Errorf("expected bearer Anthropic auth, got %q", result.anthropicAuth)
	}
	if result.models != "sensenova-6.7-flash-lite" {
		t.Errorf("expected models 'sensenova-6.7-flash-lite', got %q", result.models)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
}

func TestDetectProtocolAndModelsTreatsOpenAIShapedBearerModelsAsOpenAIWithoutMessages(t *testing.T) {
	var xAPIKeyCalls int32
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":16,"message":"Authorization Not Found"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "sensenova-6.7-flash-lite",
				"name": "SenseNova 6.7 Flash Lite",
				"created": 1783491139,
				"input_modalities": ["text"],
				"output_modalities": ["text"]
			}]
		}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL, "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "openai" {
		t.Fatalf("expected OpenAI protocol, got %q", result.protocol)
	}
	if result.anthropicAuth != "" {
		t.Fatalf("expected empty Anthropic auth, got %q", result.anthropicAuth)
	}
	if result.baseURL != server.URL+"/v1" {
		t.Fatalf("expected corrected base URL %q, got %q", server.URL+"/v1", result.baseURL)
	}
	if result.models != "sensenova-6.7-flash-lite" {
		t.Fatalf("unexpected models: %q", result.models)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 0 {
		t.Fatalf("expected bearer models success to skip x-api-key probe, got %d", got)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
}

func TestFetchModelsForProviderUsesAnthropicBearerAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-u1-fast"},{"id":"sensenova-6.7-flash-lite"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	models := fetchModelsForProvider(provider.Provider{
		Type:          "anthropic",
		Endpoint:      server.URL,
		APIKey:        "test-key",
		AnthropicAuth: anthropicAuthBearer,
	})

	if got, want := strings.Join(models, ","), "sensenova-u1-fast,sensenova-6.7-flash-lite"; got != want {
		t.Fatalf("expected models %q, got %q", want, got)
	}
}

func TestApplyModelDetectionResultNormalizesAnthropicEndpoint(t *testing.T) {
	p := provider.Provider{Endpoint: "https://token.sensenova.cn/v1"}
	m := NewAdvancedConfigModel(&p)

	_ = m.applyModelDetectionResult("anthropic", "sensenova-u1-fast", anthropicAuthBearer, "", nil)

	if p.Endpoint != "https://token.sensenova.cn" {
		t.Fatalf("expected endpoint without /v1, got %q", p.Endpoint)
	}
	if p.AnthropicAuth != anthropicAuthBearer {
		t.Fatalf("expected bearer auth, got %q", p.AnthropicAuth)
	}
	if p.Model != "sensenova-u1-fast" {
		t.Fatalf("expected detected model pool to be saved, got %q", p.Model)
	}
}

func TestApplyModelDetectionResultFailsWhenDetectionFailsWithoutExistingType(t *testing.T) {
	p := provider.Provider{
		Name:     "new-provider",
		Endpoint: "https://zenmux.ai/api",
		APIKey:   "test-key",
		Model:    "existing-model",
	}
	m := NewAdvancedConfigModel(&p)

	cmd := m.applyModelDetectionResult("", "", "", "", assertErr("models failed"))

	if m.detectionError == nil {
		t.Fatalf("expected detection error to be set")
	}
	if p.Type != "" {
		t.Fatalf("expected provider type to remain empty, got %q", p.Type)
	}
	if cmd != nil {
		t.Fatalf("expected detection failure to stay on page instead of quitting")
	}
	if m.page != 0 || m.cursor != 2 {
		t.Fatalf("expected detection failure to stay on credential page retry button, got page=%d cursor=%d", m.page, m.cursor)
	}
}

func TestApplyModelDetectionResultDoesNotFallbackToExistingPoolOnFailure(t *testing.T) {
	p := provider.Provider{
		Name:     "existing-provider",
		Type:     "openai",
		Endpoint: "https://zenmux.ai/api",
		APIKey:   "test-key",
		Model:    "existing-model",
	}
	m := NewAdvancedConfigModel(&p)

	cmd := m.applyModelDetectionResult("", "", "", "", assertErr("models failed"))

	if m.detectionError == nil {
		t.Fatalf("expected detection error instead of falling back to existing local models")
	}
	if p.Type != "openai" {
		t.Fatalf("expected provider type to be preserved, got %q", p.Type)
	}
	if len(m.modelPool) != 0 {
		t.Fatalf("expected model pool not to use existing local models, got %v", m.modelPool)
	}
	if cmd != nil {
		t.Fatalf("expected detection failure to stay on page instead of quitting")
	}
	if m.page != 0 || m.cursor != 2 {
		t.Fatalf("expected detection failure to stay on credential page retry button, got page=%d cursor=%d", m.page, m.cursor)
	}
	view := m.View()
	if !strings.Contains(view.Content, "models failed") {
		t.Fatalf("expected detection error to be visible in view, got %q", view.Content)
	}
}

func TestAdvancedConfigViewUsesCompactHeaderAndLanguageTip(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"}
	m := NewAdvancedConfigModel(&p)
	m.page = 3

	view := m.View().Content
	if !strings.Contains(view, "Reasoning Effort") || !strings.Contains(view, "Step 5/6") {
		t.Fatalf("expected compact page header, got %q", view)
	}
	if !strings.Contains(view, "Change the TUI display language") || !strings.Contains(view, "●") || !strings.Contains(view, "○") {
		t.Fatalf("expected language tip and step progress, got %q", view)
	}
}

func TestConfigModeIsSecondWorkflowStep(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"}
	m := NewAdvancedConfigModel(&p)
	m.page = 5

	view := m.View().Content
	if !strings.Contains(view, "Config Mode") || !strings.Contains(view, "Step 2/6") {
		t.Fatalf("expected config-mode page to be step 2, got %q", view)
	}
}

func TestReviewPageShowsModelMapping(t *testing.T) {
	p := provider.Provider{
		Type:          "openai",
		Endpoint:      "https://example.test/v1",
		OpusModel:     "model-opus",
		SonnetModel:   "model-sonnet",
		HaikuModel:    "model-haiku",
		CustomModelID: "model-custom",
	}
	m := NewAdvancedConfigModel(&p)
	m.page = 4
	m.oneMSlots["sonnet"] = true

	view := m.View().Content
	for _, expected := range []string{"Model Mapping", "model-opus", "model-sonnet", "model-haiku", "model-custom", "⚡1M"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("expected review mapping to contain %q, got %q", expected, view)
		}
	}
}

func TestReorderModelsByAvailability(t *testing.T) {
	models := []string{"model-unavailable", "model-available-a", "model-unknown", "model-available-b"}
	statuses := map[string]modelAvailability{
		"model-unavailable": modelAvailabilityUnavailable,
		"model-available-a": modelAvailabilityAvailable,
		"model-available-b": modelAvailabilityAvailable,
	}

	got := reorderModelsByAvailability(models, statuses)
	want := []string{"model-available-a", "model-available-b", "model-unknown", "model-unavailable"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reordered models = %v, want %v", got, want)
	}
}

func TestSlotModelAvailabilityTestUpdatesPicker(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "test-key"}
	m := NewAdvancedConfigModelAtPage1(&p, []string{"model-unavailable", "model-available"})
	m.cursor = slotTestCursor

	next, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	if !m.modelTesting || cmd == nil {
		t.Fatalf("expected availability test to start, testing=%t cmd=%v", m.modelTesting, cmd)
	}

	next, _ = m.Update(modelAvailabilityDoneMsg{statuses: map[string]modelAvailability{
		"model-unavailable": modelAvailabilityUnavailable,
		"model-available":   modelAvailabilityAvailable,
	}, testID: m.modelTestID})
	m = next.(*AdvancedConfigModel)
	if m.modelTesting {
		t.Fatal("expected availability test to finish")
	}
	if got, want := strings.Join(m.modelPool, ","), "model-available,model-unavailable"; got != want {
		t.Fatalf("model pool = %q, want %q", got, want)
	}
	if got, want := p.Model, "model-available,model-unavailable"; got != want {
		t.Fatalf("stored model pool = %q, want %q", got, want)
	}

	m.activeSlot = 0
	m.filterInput.Focus()
	m.updateFilteredPool()
	view := m.View().Content
	if !strings.Contains(view, "✓ available") || !strings.Contains(view, "✗ unavailable") {
		t.Fatalf("expected availability labels in picker, got %q", view)
	}
	if strings.Index(view, "model-available") > strings.Index(view, "model-unavailable") {
		t.Fatalf("expected available model to be listed first, got %q", view)
	}
}

func TestSlotModelAvailabilityTestCanBeCanceled(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1", APIKey: "test-key"}
	m := NewAdvancedConfigModelAtPage1(&p, []string{"model-a"})
	m.cursor = slotTestCursor

	next, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*AdvancedConfigModel)
	testID := m.modelTestID
	if !m.modelTesting || m.modelTestCancel == nil {
		t.Fatalf("expected cancelable test, testing=%t cancel=%v", m.modelTesting, m.modelTestCancel)
	}

	next, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	m = next.(*AdvancedConfigModel)
	if m.modelTesting || !m.modelTestCanceled {
		t.Fatalf("expected canceled test state, testing=%t canceled=%t", m.modelTesting, m.modelTestCanceled)
	}

	next, _ = m.Update(modelAvailabilityDoneMsg{
		testID: testID,
		statuses: map[string]modelAvailability{
			"model-a": modelAvailabilityAvailable,
		},
	})
	m = next.(*AdvancedConfigModel)
	if len(m.modelAvailability) != 0 {
		t.Fatalf("expected canceled test result to be ignored, got %v", m.modelAvailability)
	}
	if view := m.View().Content; !strings.Contains(view, "Test canceled; results were not applied") {
		t.Fatalf("expected cancellation feedback, got %q", view)
	}
}

func TestAdvancedConfigViewAdaptsToWindowSize(t *testing.T) {
	p := provider.Provider{Type: "openai", Endpoint: "https://example.test/v1"}
	m := NewAdvancedConfigModel(&p)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 48})
	m = next.(*AdvancedConfigModel)

	if got, want := m.panelWidth(), preferredPanelWidth; got != want {
		t.Fatalf("panel width = %d, want %d", got, want)
	}
	if got, want := m.urlInput.Width(), preferredPanelWidth-8; got != want {
		t.Fatalf("URL input width = %d, want %d", got, want)
	}
	if got := m.View().Content; !strings.Contains(got, "Change the TUI display language") {
		t.Fatalf("expected footer in resized view, got %q", got)
	}
}

func TestApplyModelDetectionResultUsesDiscoveredModelsOnly(t *testing.T) {
	p := provider.Provider{
		Name:          "existing-provider",
		Type:          "openai",
		Endpoint:      "https://zenmux.ai/api/v1",
		APIKey:        "test-key",
		Model:         "old-a,old-b",
		OpusModel:     "old-a",
		SonnetModel:   "new-b",
		HaikuModel:    "old-haiku",
		CustomModelID: "new-a",
	}
	m := NewAdvancedConfigModel(&p)

	_ = m.applyModelDetectionResult("openai", "new-a,new-b", "", "", nil)

	if m.detectionError != nil {
		t.Fatalf("unexpected detection error: %v", m.detectionError)
	}
	if p.Model != "new-a,new-b" {
		t.Fatalf("expected local model pool to be refreshed from API models, got %q", p.Model)
	}
	if strings.Join(m.modelPool, ",") != "new-a,new-b" {
		t.Fatalf("expected selectable model pool to use API models only, got %v", m.modelPool)
	}
	if m.staleSlotCount() != 2 {
		t.Fatalf("expected two stale slot mappings, got %d", m.staleSlotCount())
	}
	m.applyStaleSlotPolicy()
	if p.OpusModel != "" || p.HaikuModel != "" {
		t.Fatalf("expected stale slot mappings to be cleared, got %+v", p)
	}
	if p.SonnetModel != "new-b" || p.CustomModelID != "new-a" {
		t.Fatalf("expected slot mappings present in API list to be kept, got %+v", p)
	}
}

func TestDetectProtocolAndModelsDefaultsOpenAIToChatEvenWhenResponsesWorks(t *testing.T) {
	server := newMockGatewayServer(t, []string{"gpt-5"}, true)

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Errorf("expected protocol 'openai' (openai(chat)), got %q", proto)
	}
	if models != "gpt-5" {
		t.Errorf("expected models 'gpt-5', got %q", models)
	}
}

func TestDetectProtocolAndModelsNonVersionProbeFallsBackToXAPIKeyV1Models(t *testing.T) {
	var bearerCalls int32
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			atomic.AddInt32(&bearerCalls, 1)
			http.Error(w, "bearer unsupported", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&xAPIKeyCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "claude-router",
				"type": "model",
				"created_at": "2026-07-09T00:00:00Z"
			}],
			"has_more": false
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/api", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "anthropic" || models != "claude-router" {
		t.Fatalf("expected Anthropic x-api-key detection, got proto=%q models=%q", proto, models)
	}
	if got := atomic.LoadInt32(&bearerCalls); got != 1 {
		t.Fatalf("expected one bearer probe before x-api-key fallback, got %d", got)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 1 {
		t.Fatalf("expected one x-api-key shape probe, got %d", got)
	}
}

func TestDetectProtocolAndModelsFallsBackToBearerWhenV1Missing(t *testing.T) {
	var xAPIKeyCalls int32
	var bearerCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
			http.Error(w, "x-api-key unsupported", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") == "Bearer test-key" {
			atomic.AddInt32(&bearerCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5","object":"model"}]}`))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL+"/api", "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "openai" || result.models != "gpt-5" {
		t.Fatalf("expected OpenAI bearer fallback, got proto=%q models=%q", result.protocol, result.models)
	}
	if result.baseURL != server.URL+"/api/v1" {
		t.Fatalf("expected corrected base URL %q, got %q", server.URL+"/api/v1", result.baseURL)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 0 {
		t.Fatalf("expected bearer success to skip x-api-key fallback, got %d", got)
	}
	if got := atomic.LoadInt32(&bearerCalls); got != 1 {
		t.Fatalf("expected one bearer probe, got %d", got)
	}
}

func TestDetectProtocolAndModelsTriesXAPIKeyForAnthropicBasePath(t *testing.T) {
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("non-version x-api-key probe should not send bearer")
		}
		atomic.AddInt32(&xAPIKeyCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "deepseek-v4-pro",
				"type": "model",
				"display_name": "DeepSeek V4 Pro",
				"created_at": "2026-07-08T00:00:00Z"
			}],
			"has_more": false
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/anthropic", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "anthropic" {
		t.Fatalf("expected anthropic protocol, got %q", proto)
	}
	if models != "deepseek-v4-pro" {
		t.Fatalf("unexpected models: %q", models)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 1 {
		t.Fatalf("expected one x-api-key probe, got %d", got)
	}
}

func TestDetectProtocolAndModelsFailsAnthropicSuffixWithoutModelList(t *testing.T) {
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/anthropic", "test-key")
	if err == nil {
		t.Fatalf("expected detection error when /anthropic/v1/models is unavailable")
	}
	if proto != "" || models != "" {
		t.Fatalf("expected empty protocol/models on failure, got proto=%q models=%q", proto, models)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 1 {
		t.Fatalf("expected one x-api-key probe, got %d", got)
	}
}

func TestDetectProtocolAndModelsShapeProbeDetectsAnthropicShape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "bearer unsupported", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "claude-router",
				"type": "model",
				"created_at": "2026-07-09T00:00:00Z"
			}],
			"hasMore": false
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL+"/api", "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "anthropic" {
		t.Fatalf("expected Anthropic from response shape, got %q", result.protocol)
	}
	if result.anthropicAuth != anthropicAuthXAPIKey {
		t.Fatalf("expected x-api-key auth, got %q", result.anthropicAuth)
	}
	if result.models != "claude-router" {
		t.Fatalf("unexpected models: %q", result.models)
	}
}

func TestDetectProtocolAndModelsRequiresAnthropicPathSuffix(t *testing.T) {
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/proxy/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	_, _, err := detectProtocolAndModels(server.URL+"/anthropic/proxy", "test-key")
	if err == nil {
		t.Fatalf("expected detection error")
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 1 {
		t.Fatalf("expected one combined shape probe when anthropic is not the path suffix, got %d", got)
	}
}

func TestDetectProtocolAndModelsTreatsVersionSuffixAsOpenAI(t *testing.T) {
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-4.5","object":"model"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/api/v4", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" || models != "glm-4.5" {
		t.Fatalf("expected OpenAI version suffix detection, got proto=%q models=%q", proto, models)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 0 {
		t.Fatalf("expected no x-api-key for version suffix, got %d", got)
	}
}

func TestDetectProtocolAndModelsTreatsVersionModelsURLAsVersionSuffix(t *testing.T) {
	var xAPIKeyCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			atomic.AddInt32(&xAPIKeyCalls, 1)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-4.5","object":"model"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/api/v4/models", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" || models != "glm-4.5" {
		t.Fatalf("expected OpenAI version models URL detection, got proto=%q models=%q", proto, models)
	}
	if got := atomic.LoadInt32(&xAPIKeyCalls); got != 0 {
		t.Fatalf("expected no x-api-key for version models URL, got %d", got)
	}
}

func TestDetectProtocolAndModelsDefaultsAmbiguousBearerModelsToOpenAI(t *testing.T) {
	var chatCalls int32
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chatCalls, 1)
		http.Error(w, "chat should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Fatalf("expected OpenAI Chat, got %q", proto)
	}
	if models != "gpt-4o" {
		t.Fatalf("expected OpenAI models to be kept, got %q", models)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
	if got := atomic.LoadInt32(&chatCalls); got != 0 {
		t.Fatalf("expected no /chat/completions probes during setup, got %d", got)
	}
}

func TestDetectProtocolAndModelsTreatsHybridModelListAsOpenAI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" && r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "kuaishou/kat-coder-air-v2.5",
				"object": "model",
				"display_name": "KwaiKAT: KAT-Coder-Air-V2.5",
				"created": 1783491139,
				"owned_by": "kuaishou"
			}]
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Fatalf("expected OpenAI for hybrid OpenAI-shaped model list, got %q", proto)
	}
	if models != "kuaishou/kat-coder-air-v2.5" {
		t.Fatalf("unexpected models: %q", models)
	}
}

func TestDetectProtocolAndModelsDoesNotProbeAnthropicMessagesForAmbiguousBearerModels(t *testing.T) {
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Errorf("expected protocol 'openai', got %q", proto)
	}
	if models != "gpt-5.5" {
		t.Errorf("expected models 'gpt-5.5', got %q", models)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
}

func TestDetectProtocolAndModelsDoesNotProbeAnthropicBearerMessagesWithoutAnthropicModels(t *testing.T) {
	var messageCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "Authorization Not Found", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sensenova-6.7-flash-lite"}]}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messageCalls, 1)
		http.Error(w, "messages should not be probed during setup", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := detectProtocolAndModelsDetailed(server.URL+"/v1", "test-key")
	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if result.protocol != "openai" {
		t.Errorf("expected protocol 'openai', got %q", result.protocol)
	}
	if result.models != "sensenova-6.7-flash-lite" {
		t.Errorf("expected models 'sensenova-6.7-flash-lite', got %q", result.models)
	}
	if got := atomic.LoadInt32(&messageCalls); got != 0 {
		t.Fatalf("expected no /messages probes during setup, got %d", got)
	}
}

func TestDetectProtocolAndModelsFallsBackToOpenAIChat(t *testing.T) {
	server := newMockGatewayServer(t, []string{"gpt-4o"}, false)

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "openai" {
		t.Errorf("expected protocol 'openai' (openai(chat)), got %q", proto)
	}
	if models != "gpt-4o" {
		t.Errorf("expected models 'gpt-4o', got %q", models)
	}
}

func TestDetectProtocolAndModelsBothFail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "bad-key")
	if err == nil {
		t.Fatalf("expected error when both protocols fail")
	}
	if models != "" {
		t.Errorf("expected empty models on failure, got %q", models)
	}
	if proto != "" {
		t.Errorf("expected empty protocol on failure, got %q", proto)
	}
	if !strings.Contains(err.Error(), "unsupported protocol") && !strings.Contains(err.Error(), "暂不支持这个协议") {
		t.Errorf("expected unsupported protocol error, got %q", err.Error())
	}
}

func TestDetectProtocolAndModelsAcceptsModelsOnlyGateway(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proto, models, err := detectProtocolAndModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatalf("unexpected error for models-only gateway: %v", err)
	}
	if proto != "openai" || models != "gpt-5.5" {
		t.Fatalf("expected OpenAI models-only gateway, got proto=%q models=%q", proto, models)
	}
}

func TestParseModelListForDetectionInfersResponseShapes(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModels string
		wantShape  modelListShape
	}{
		{
			name: "anthropic model list",
			body: `{
				"data": [{
					"id": "claude-3-5-sonnet",
					"type": "model",
					"display_name": "Claude 3.5 Sonnet",
					"created_at": "2024-06-20T00:00:00Z"
				}],
				"has_more": false
			}`,
			wantModels: "claude-3-5-sonnet",
			wantShape:  modelListShapeAnthropic,
		},
		{
			name: "openai model list",
			body: `{
				"object": "list",
				"data": [{
					"id": "gpt-5",
					"object": "model",
					"display_name": "GPT 5",
					"created": 1780000000,
					"owned_by": "openai"
				}]
			}`,
			wantModels: "gpt-5",
			wantShape:  modelListShapeOpenAI,
		},
		{
			name:       "minimal model list",
			body:       `{"data":[{"id":"router-model"}]}`,
			wantModels: "router-model",
			wantShape:  modelListShapeUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelListForDetection([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseModelListForDetection failed: %v", err)
			}
			if got.models != tc.wantModels {
				t.Fatalf("models = %q, want %q", got.models, tc.wantModels)
			}
			if got.shape != tc.wantShape {
				t.Fatalf("shape = %q, want %q", got.shape, tc.wantShape)
			}
		})
	}
}
