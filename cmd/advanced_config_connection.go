package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// saveWouldReplaceModelsDev reports whether saving right now would silently
// replace a configured models.dev provider: the active source is Custom, the
// Custom draft shares its name with the models.dev side, and that side carries
// the per-model protocol table that would be lost.
func (m *AdvancedConfigModel) saveWouldReplaceModelsDev() bool {
	if m.source != sourceCustom || m.modelsDevDraft == nil || m.modelsDevDraft.p == nil {
		return false
	}
	other := m.modelsDevDraft.p
	return strings.EqualFold(strings.TrimSpace(other.Name), strings.TrimSpace(m.p.Name)) &&
		provider.IsModelsDevType(other.Type) && len(other.ModelProtocols) > 0
}

// configureOAuthRuntime adopts the loopback runtime ccl started for this page.
// A subscription has no connection to probe, so there is no Auto Configure row:
// the runtime owns the catalog, and it is copied in directly rather than
// fetched back over HTTP. This is also where the page becomes editable, so it
// is the single "the runtime is up" transition.
// catalogFallback reports that the runtime could not fetch the account's models
// and is serving a built-in compatibility list. The page then keeps the catalog
// it already has: adopting the fallback is what silently shrank a saved Qoder
// list from 17 models to 5 after the credential expired, and saving the page
// made that permanent.
func (m *AdvancedConfigModel) configureOAuthRuntime(endpoint, apiKey string, models []string, catalogFallback bool) {
	m.live().probeEndpoint = endpoint
	m.live().probeAPIKey = apiKey
	m.live().connectionDirty = false
	m.runtimeReady = true
	m.runtimeCatalogFallback = catalogFallback
	if catalog := uniqueModels(models); len(catalog) > 0 && !catalogFallback {
		// Same result the probe used to produce: pool from the runtime, Model
		// persisted, empty slots auto-mapped. The endpoint and key stay in
		// live() only — a subscription's persisted Endpoint is oauth://.
		m.applyModelDetectionResult("", strings.Join(catalog, ","), "", "", nil)
	}
	m.urlFocused = false
	m.keyFocused = false
}

// beginOAuthRuntime starts the subscription's loopback runtime off the UI
// thread. Starting it used to happen before the panel existed, which left the
// terminal blank while the runtime refreshed its credential and fetched its
// catalog; now the panel renders first and the Local Proxy row shows a spinner
// until the result lands.
func (m *AdvancedConfigModel) beginOAuthRuntime(p provider.Provider) {
	ctx, cancel := context.WithCancel(context.Background())
	m.oauthCancel = cancel
	m.runtimeLoading = true
	m.runtimeReady = false
	m.runtimeErr = nil
	go func() {
		defer close(m.oauthStartDone)
		runtimeProvider, runtime, cleanup, err := prepareProviderRuntime(ctx, p)
		if err != nil {
			m.oauthDone <- oauthRuntimeDoneMsg{err: err}
			return
		}
		m.runtimeCleanup = cleanup
		var modelInfos []protocol.ModelInfo
		if strings.EqualFold(strings.TrimSpace(p.OAuthProvider), "qoder") {
			// Qoder publishes account-specific credit multipliers on the local
			// catalog. Preserve them alongside display names for the picker.
			modelInfos = fetchModelInfosForProvider(runtimeProvider)
		}
		m.oauthDone <- oauthRuntimeDoneMsg{
			endpoint:        runtimeProvider.Endpoint,
			apiKey:          runtimeProvider.APIKey,
			models:          runtime.Models(),
			names:           runtime.ModelDisplayNames(),
			modelInfos:      modelInfos,
			catalogFallback: runtime.ModelCatalogIsFallback(),
		}
	}()
}

// stopOAuthRuntime cancels a start that is still in flight and tears down a
// runtime that came up. It is deferred in RunProviderSet, so it covers every
// exit path including an early return from app.Run. Session.Close and
// Runtime.Stop are both idempotent.
func (m *AdvancedConfigModel) stopOAuthRuntime() {
	if m.oauthCancel == nil {
		return
	}
	m.oauthCancel()
	select {
	case <-m.oauthStartDone:
		if m.runtimeCleanup != nil {
			m.runtimeCleanup()
		}
	case <-time.After(3 * time.Second):
		// The start is wedged on the network despite the cancel. Leaving the
		// goroutine behind is safe: the process is on its way out.
		setDebugf("oauth runtime start did not finish before exit")
	}
}

func (m *AdvancedConfigModel) usesOAuth() bool {
	return m.p != nil && strings.TrimSpace(m.p.OAuthProvider) != ""
}

// usesModelsDev reports whether the active Connection source is the models.dev
// picker. Its endpoint and per-model protocols come from metadata, not from an
// HTTP probe, so connection readiness and dirty tracking are bypassed.
func (m *AdvancedConfigModel) usesModelsDev() bool {
	return m.source == sourceModelsDev
}

// openModelsDevPicker opens the models.dev provider overlay and starts the
// catalog fetch.
func (m *AdvancedConfigModel) openModelsDevPicker() {
	m.urlFocused = false
	m.keyFocused = false
	m.live().detecting = false
	m.modelsDevPicker = true
	m.modelsDevLoading = true
	m.modelsDevError = nil
	m.modelsDevItems = nil
	m.modelsDevFiltered = nil
	m.modelsDevCursor = 0
	m.modelsDevWindow = 0
	m.modelsDevText.Set("")
}

func (m *AdvancedConfigModel) closeModelsDevPicker() {
	m.modelsDevPicker = false
}

// updateModelsDevFilter recomputes the filtered provider list from the filter
// input and clamps cursor/window. Filtering matches name and id case-insensitively.
func (m *AdvancedConfigModel) updateModelsDevFilter() {
	q := strings.ToLower(strings.TrimSpace(m.modelsDevText.Get()))
	if q == "" {
		m.modelsDevFiltered = m.modelsDevItems
	} else {
		m.modelsDevFiltered = make([]modelsdev.Provider, 0, len(m.modelsDevItems))
		for _, p := range m.modelsDevItems {
			if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.ID), q) {
				m.modelsDevFiltered = append(m.modelsDevFiltered, p)
			}
		}
	}
	if m.modelsDevCursor >= len(m.modelsDevFiltered) {
		m.modelsDevCursor = len(m.modelsDevFiltered) - 1
	}
	if m.modelsDevCursor < 0 {
		m.modelsDevCursor = 0
	}
	m.modelsDevWindow = 0
}

// saveInputsToDraft copies the shared text-widget values into the current
// source's input mirror, so switching away and back preserves what the user
// typed. It does not touch p.Endpoint/APIKey: those hold the normalized or
// metadata values, while the mirror keeps the raw text.
func (m *AdvancedConfigModel) saveInputsToDraft() {
	if m.usesOAuth() {
		return
	}
	m.live().inputEndpoint = m.urlText.Get()
	m.live().inputAPIKey = m.keyText.Get()
}

// otherSource returns the source to toggle to (Custom ↔ models.dev).
func (m *AdvancedConfigModel) otherSource() connectionSource {
	if m.source == sourceModelsDev {
		return sourceCustom
	}
	return sourceModelsDev
}

// switchSource flips the Connection source, preserving each side's draft. The
// current side's text inputs are saved to its mirror, the pointer rebinds to the
// target draft, and the shared widget values refresh from the target mirror.
func (m *AdvancedConfigModel) switchSource(target connectionSource) {
	if m.source == target || (m.live().detecting && !m.usesModelsDev()) {
		return
	}
	if m.usesModelsDev() && m.live().detecting {
		m.live().detecting = false
		m.live().verifyGeneration++
		m.live().autoVerifyPending = !m.live().keyVerified && strings.TrimSpace(m.keyText.Get()) != ""
	}
	// Abort an in-flight availability test: its results belong to the old pool.
	if m.modelTesting && m.modelTestCancel != nil {
		m.modelTestCancel()
	}
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	m.modelAvailability = make(map[string]modelAvailability)

	m.saveInputsToDraft()
	m.source = target
	m.p = m.live().p
	// Leaving the Custom side abandons any pending overwrite warning.
	m.customDraft.saveGuardPending = false

	m.urlText.Set(m.live().inputEndpoint)
	m.keyText.Set(m.live().inputAPIKey)
	m.urlFocused = false
	m.keyFocused = false
	m.filterFocused = false
	m.updateFilteredPool()
	m.cursor = m.mainRowIndex(rowSource)
	m.keepCursorVisible()
	if m.usesModelsDev() {
		m.startModelsDevVerification()
	}
}

// applyModelsDevProvider fills the models.dev draft with the chosen provider and
// switches to it. Endpoint, per-model protocol table, model pool, and slot
// recommendation all come from metadata; only the API key remains to be entered.
// The Custom draft is left untouched so the user can switch back.
func (m *AdvancedConfigModel) applyModelsDevProvider(p modelsdev.Provider) {
	draft, metadata := modelsDevProviderToDraft(p)
	if name := strings.TrimSpace(m.providerName); name != "" && !m.NameGenerated {
		draft.Name = name
	}
	d := m.modelsDevDraft
	*d.p = draft
	d.modelPool = uniqueModels(parseModelList(draft.Model))
	d.modelPoolFromDiscovery = true
	d.modelDisplayMetadata = copyModelInfoIndex(metadata)
	d.modelContextWindows = contextWindowsFromModelInfos(d.modelDisplayMetadata)
	d.detectedInputEndpoint = draft.Endpoint
	d.detectedInputKey = ""
	d.connectionDirty = false
	d.autoDetectOnOpen = false
	d.detectionError = nil
	d.keyVerified = false
	d.autoVerifyPending = false
	d.lastKeyEditAt = time.Time{}
	d.verifyGeneration++
	d.probeEndpoint = draft.Endpoint
	d.probeAPIKey = ""
	d.inputEndpoint = draft.Endpoint
	d.inputAPIKey = ""

	if m.source != sourceModelsDev {
		m.saveInputsToDraft()
		m.source = sourceModelsDev
		m.p = d.p
		// The overwrite warning belonged to the Custom side being left behind.
		m.customDraft.saveGuardPending = false
	}
	m.applyRecommendation()
	m.urlText.Set(d.inputEndpoint)
	m.keyText.Set(d.inputAPIKey)
	m.urlFocused = false
	m.keyFocused = true
	m.cursor = m.mainRowIndex(rowAPIKey)
	setDebugf("models.dev applied provider=%q model_count=%d protocols=%d", draft.Name, len(d.modelPool), len(draft.ModelProtocols))
}

// startAutoDetect re-verifies an existing provider's connection on open. Until
// the check succeeds the sections below Connection stay greyed out; OAuth
// providers are always ready so they skip this. Called once from Watchers()
// startup (go-tui has no Init() on the SetRootComponent path).
func (m *AdvancedConfigModel) startAutoDetect() {
	if m.usesModelsDev() {
		m.startModelsDevVerification()
		return
	}
	if m.live().autoDetectOnOpen && !m.usesOAuth() && !m.usesModelsDev() && strings.TrimSpace(m.live().probeEndpoint) != "" {
		m.live().detecting = true
		m.live().detectProgress = 5
		m.live().detectFrame = 0
		fetchModelsAsync(m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey, m.p.Type, m.p.AnthropicAuth)
	}
}

// startModelsDevVerification checks only the credential. Metadata already
// supplies the endpoint, model catalog, and per-model routing, so running full
// protocol detection here would destroy that configuration.
func (m *AdvancedConfigModel) startModelsDevVerification() {
	if !m.usesModelsDev() || !m.live().autoVerifyPending || m.live().detecting {
		return
	}
	key := strings.TrimSpace(m.keyText.Get())
	if key == "" || strings.TrimSpace(m.p.Endpoint) == "" {
		return
	}
	m.live().autoVerifyPending = false
	model, proto, ok := m.firstRoutableModel()
	if !ok {
		m.live().detectionError = fmt.Errorf("%s", locale.T(
			"该 Provider 没有可用模型（AI SDK 包暂不支持），无法验证 key",
			"this provider has no usable models (unsupported AI SDK package); cannot verify the key",
		))
		m.live().keyVerified = false
		setDebugf("models.dev key verify aborted: no routable model endpoint=%q", m.p.Endpoint)
		m.markDirty()
		return
	}
	m.live().probeEndpoint = m.p.Endpoint
	m.live().probeAPIKey = key
	m.live().detectionError = nil
	m.live().keyVerified = false
	m.live().detecting = true
	m.live().detectProgress = 5
	m.live().detectFrame = 0
	setDebugf("start models.dev key verify endpoint=%q api_key_len=%d model=%q proto=%q", m.p.Endpoint, len(key), model, proto)
	m.live().verifyGeneration++
	keyVerifyAsync(m.verifyDone, m.p.Endpoint, key, model, proto, m.live().verifyGeneration)
	m.markDirty()
}

// A failed automatic check may have been a transient network/server failure.
// Enter on API Key explicitly retries without forcing the user to alter a
// valid credential just to re-arm the automatic verifier.
func (m *AdvancedConfigModel) retryModelsDevVerification() {
	if !m.usesModelsDev() || m.live().keyVerified || m.live().detecting {
		return
	}
	m.live().autoVerifyPending = strings.TrimSpace(m.keyText.Get()) != ""
	m.startModelsDevVerification()
}

// invalidateModelsDevKeyIfChanged drops a stale key verification once the key
// text changes: keyVerified only proves the key that was verified at the
// time, and content changes void it. If a verify request is still in flight
// the probe baseline is resynced too, so a late keyVerifyDoneMsg cannot slip
// past the guard and mark the unverified new key as connected.
func (m *AdvancedConfigModel) invalidateModelsDevKeyIfChanged() {
	if m.modelTesting && m.modelTestCancel != nil {
		m.modelTestCancel()
	}
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelAvailability = make(map[string]modelAvailability)
	m.live().keyVerified = false
	m.live().detectionError = nil
	m.live().detecting = false
	m.live().probeAPIKey = m.keyText.Get()
	m.live().verifyGeneration++
	m.live().autoVerifyPending = strings.TrimSpace(m.keyText.Get()) != ""
	m.live().lastKeyEditAt = time.Now()
}
