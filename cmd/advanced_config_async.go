package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// keyVerifyAsync sends one minimal, authenticated inference request for the
// given model and protocol, and reports only whether the key was accepted at
// the auth layer. Unlike fetchModelsAsync it never mutates provider config —
// the metadata-derived Type, endpoint, and per-model protocol table are left
// untouched; only the key's validity (keyVerified) is recorded. The result is
// delivered on the verify channel consumed by Watchers().
func keyVerifyAsync(done chan<- keyVerifyDoneMsg, endpoint, apiKey, model, proto string, generation uint64) {
	go func() {
		err := verifyProviderAPIKey(context.Background(), model, endpoint, apiKey, proto, keyVerifyTimeout)
		done <- keyVerifyDoneMsg{endpoint: endpoint, apiKey: apiKey, generation: generation, err: err}
	}()
}

// verifyProviderAPIKey sends a minimal authenticated inference request for one
// model in the provider's protocol table and reports whether the key is
// rejected. A 401/403 means the key itself is invalid; a 2xx or any other 4xx
// (bad model/params, rate limit) proves the key passed auth. Transport errors
// and 5xx are reported as "cannot verify" rather than "verified".
func verifyProviderAPIKey(parent context.Context, model, endpoint, apiKey, proto string, timeout time.Duration) error {
	var status int
	var err error
	switch proto {
	case "anthropic":
		status, err = probeModelStatus(parent, buildAnthropicMessagesURL(endpoint), map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"anthropic-version": "2023-06-01", "x-api-key": apiKey}, timeout)
	case "openai_responses":
		status, err = protocol.ProbeOpenAIResponsesStatusContext(parent, endpoint, apiKey, model, timeout)
	default: // "openai" (Chat Completions)
		status, err = probeSingleOpenAIModelStatusContext(parent, model, endpoint, apiKey, timeout)
	}
	if err != nil {
		return err
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf(locale.T("API key 无效（HTTP %d）", "invalid API key (HTTP %d)"), status)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if status >= 400 && status < 500 {
		// Auth already passed; the model/params/rate-limit rejected the request.
		return nil
	}
	return fmt.Errorf(locale.T("验证请求失败（HTTP %d）", "verification request failed (HTTP %d)"), status)
}

// firstRoutableModel returns the model used to send a real key-verification
// request: the first pool entry with a protocol in the provider's per-model
// table, or, when no entry covers the pool, the first pool entry over the
// runtime's default wire protocol. ok is false only for an empty pool.
//
// The fallback matters for a gateway whose models.dev catalog entry is a
// curated subset under different IDs than its live /models list (ClinePass
// serves a large meta-router catalog, models.dev lists 18 branded models).
// Such a pool is fully routable — the mixed runtime sends an unlisted model
// over Chat Completions — so refusing to verify it would report a working
// provider as unusable and block saving it.
func (m *AdvancedConfigModel) firstRoutableModel() (model, proto string, ok bool) {
	var fallback string
	for _, name := range m.live().modelPool {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if fallback == "" {
			fallback = trimmed
		}
		if proto := m.p.ModelProtocols[strings.ToLower(trimmed)]; proto != "" {
			return trimmed, proto, true
		}
	}
	if fallback == "" {
		return "", "", false
	}
	return fallback, probeProtocolForModel(m.p.ModelProtocols, fallback), true
}

// fetchModelsDevAsync fetches the models.dev catalog off the UI thread and
// delivers the result on the models.dev channel.
func fetchModelsDevAsync(done chan<- modelsDevFetchDoneMsg) {
	go func() {
		providers, err := modelsDevProviders(context.Background())
		done <- modelsDevFetchDoneMsg{providers: providers, err: err}
	}()
}

// fetchModelsDevRefreshAsync refreshes the catalog for an already-selected
// models.dev provider. The page opens on the persisted model pool and never
// re-probes it (a probe would destroy the per-model routing), so without this
// the pool is a snapshot from whenever the provider was last saved: models the
// catalog dropped stay selectable and new ones never appear. The result merges
// additively in handleModelsDevRefreshDone; a failed fetch just keeps the
// saved list.
func fetchModelsDevRefreshAsync(done chan<- modelsDevFetchDoneMsg, providerID string) {
	go func() {
		providers, err := modelsDevProviders(context.Background())
		done <- modelsDevFetchDoneMsg{providers: providers, err: err, refreshFor: providerID}
	}()
}

// fetchModelsAsync runs protocol detection + model fetching off the UI thread
// and delivers the result on the fetch channel.
func fetchModelsAsync(done chan<- modelFetchDoneMsg, endpoint, apiKey string, preference ...string) {
	go func() {
		setDebugf("modelFetch start endpoint=%q api_key_len=%d", endpoint, len(apiKey))
		result := detectProtocolAndModelsPreferred(endpoint, apiKey, preference...)
		setDebugf(
			"modelFetch done endpoint=%q detected_endpoint=%q protocol=%q anthropic_auth=%q model_count=%d err=%v",
			endpoint,
			result.baseURL,
			result.protocol,
			result.anthropicAuth,
			countCSV(result.models),
			result.err,
		)
		// Best-effort: pull context_window metadata for OpenAI-family catalogs.
		// Failures are ignored — IDs still come from detection.
		windows := map[string]int{}
		if result.err == nil && result.protocol != "" && !provider.IsAnthropicType(result.protocol) {
			// Subscription runtimes only expose windows through the Codex catalog,
			// which AdvertisedContextWindows tries before the plain OpenAI list.
			advertised, source := claude.AdvertisedContextWindows(result.baseURL, apiKey)
			for id, window := range advertised {
				windows[id] = window
			}
			setDebugf("modelFetch context windows catalog=%q count=%d", source, len(windows))
		}
		done <- modelFetchDoneMsg{
			endpoint:            endpoint,
			apiKey:              apiKey,
			detectedType:        result.protocol,
			detectedEndpoint:    result.baseURL,
			anthropicAuth:       result.anthropicAuth,
			discoveredModelsRaw: result.models,
			contextWindows:      windows,
			modelInfos:          result.modelInfos,
			err:                 result.err,
		}
	}()
}

// testModelsAsync probes every model in the pool concurrently and delivers the
// statuses on the availability channel. A rejected probe is not necessarily
// evidence that the model itself is unavailable; keyVerified lets a verified
// models.dev source upgrade a 404 to a concrete "unavailable" verdict.
func testModelsAsync(done chan<- modelAvailabilityDoneMsg, ctx context.Context, testID uint64, models []string, endpoint, apiKey, providerType, anthropicAuth string, protocols map[string]string, smokeTestModel string, keyVerified bool) {
	models = append([]string(nil), models...)
	go func() {
		statuses := make(map[string]modelAvailability, len(models))
		if len(models) == 0 {
			done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
			return
		}
		if smokeTestModel != "" {
			status := probeModelAvailability(ctx, smokeTestModel, endpoint, apiKey, providerType, anthropicAuth, protocols, keyVerified)
			if ctx.Err() == nil {
				for _, model := range models {
					statuses[model] = status
				}
			}
			done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
			return
		}

		jobs := make(chan string, len(models))
		for _, model := range models {
			jobs <- model
		}
		close(jobs)

		var wg sync.WaitGroup
		var mu sync.Mutex
		workers := min(slotTestConcurrency, len(models))
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case model, ok := <-jobs:
						if !ok {
							return
						}
						status := probeModelAvailability(ctx, model, endpoint, apiKey, providerType, anthropicAuth, protocols, keyVerified)
						if ctx.Err() != nil {
							return
						}
						mu.Lock()
						statuses[model] = status
						mu.Unlock()
					}
				}
			}()
		}

		wg.Wait()
		done <- modelAvailabilityDoneMsg{testID: testID, statuses: statuses}
	}()
}

// A non-2xx response does not establish model availability by itself: even a
// 404 can mean the endpoint path is wrong rather than the model. Once the API
// key has been verified against this endpoint, though, a 404 on a known-correct
// path is real evidence the model is gone, so it becomes "unavailable". Other
// 4xx stay inconclusive: 400 can be a parameter mismatch and 429 a rate limit.
// keyVerified only flips the 404 verdict for models.dev-style sources whose
// endpoint and per-model protocol table come from catalog metadata.
func probeModelAvailability(ctx context.Context, model, endpoint, apiKey, providerType, anthropicAuth string, protocols map[string]string, keyVerified bool) modelAvailability {
	const attempts = 3
	for attempt := range attempts {
		status, err := probeSingleModelWithProtocolsStatusContext(ctx, model, endpoint, apiKey, providerType, anthropicAuth, protocols, modelProbeTimeout)
		if err != nil || status == 0 {
			return modelAvailabilityInconclusive
		}
		switch {
		case status >= 200 && status < 300:
			return modelAvailabilityAvailable
		case keyVerified && status == http.StatusNotFound:
			return modelAvailabilityUnavailable
		case status != http.StatusTooManyRequests && status < 500:
			return modelAvailabilityInconclusive
		}
		if attempt == attempts-1 || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return modelAvailabilityInconclusive
		case <-timer.C:
		}
	}
	return modelAvailabilityInconclusive
}
