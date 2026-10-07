package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// handleOAuthRuntimeDone folds the background start into the page. A failure is
// reported on the Local Proxy row rather than swallowed: without a runtime the
// page has no catalog, and the save gate stays shut.
func (m *AdvancedConfigModel) handleOAuthRuntimeDone(msg oauthRuntimeDoneMsg) {
	m.runtimeLoading = false
	if msg.err != nil {
		m.runtimeErr = msg.err
		m.runtimeReady = false
		setDebugf("oauth runtime start failed err=%v", msg.err)
		m.markDirty()
		return
	}
	m.runtimeErr = nil
	setDebugf("oauth runtime ready endpoint=%q model_count=%d catalog_fallback=%t",
		msg.endpoint, len(msg.models), msg.catalogFallback)
	if msg.catalogFallback {
		// The runtime is usable — Claude Code can still send requests through it —
		// but its model list is a guess. Keeping the saved pool means a page that
		// opens on a broken credential is not a page that quietly deletes the
		// account's real models when it is saved.
		setDebugf("oauth runtime catalog unavailable; keeping the saved model pool")
	}
	m.configureOAuthRuntime(msg.endpoint, msg.apiKey, msg.models, msg.catalogFallback)
	m.setRuntimeModelNames(msg.names)
	m.markDirty()
}

func (m *AdvancedConfigModel) applyModelDetectionResult(detectedType, discoveredModelsRaw, anthropicAuth, detectedEndpoint string, derr error) {
	discoveredModels := uniqueModels(parseModelList(discoveredModelsRaw))
	m.live().hadLocalModelPool = countCSV(m.p.Model) > 0
	// Capture the raw detection inputs before probeEndpoint is normalized below,
	// so the successful-detection baseline is what the user actually typed.
	detectedInputEndpoint := m.live().probeEndpoint
	detectedInputKey := m.live().probeAPIKey
	setDebugf(
		"applyModelDetectionResult start detected_type=%q detected_endpoint=%q anthropic_auth=%q discovered_model_count=%d existing_model_count=%d err=%v",
		detectedType,
		detectedEndpoint,
		anthropicAuth,
		len(discoveredModels),
		countCSV(m.p.Model),
		derr,
	)
	if !m.usesOAuth() && detectedEndpoint != "" {
		m.p.Endpoint = detectedEndpoint
		m.live().probeEndpoint = detectedEndpoint
	}
	if !m.usesOAuth() && detectedType != "" {
		// The probe can only tell apart the Anthropic and OpenAI families — it
		// cannot distinguish OpenAI's Chat Completions from its Responses API
		// (both are reached through the same GET /v1/models list).
		// Reopening an existing openai_responses provider must not let the
		// auto-probe downgrade its stored Type back to "openai" (chat); preserve
		// the user's Responses choice when the probe still lands in the OpenAI
		// family, and only overwrite the Type when the probe family disagrees.
		if !(provider.IsOpenAIResponsesType(m.p.Type) && provider.IsOpenAICompatibleType(detectedType)) {
			m.p.Type = detectedType
			m.p.AnthropicAuth = ""
		}
	}
	if !m.usesOAuth() && detectedType == "anthropic" {
		m.p.Endpoint = protocol.NormalizeAnthropicBaseURLForClaude(m.p.Endpoint)
		if anthropicAuth != "" {
			m.p.AnthropicAuth = anthropicAuth
		}
		m.live().anthropicAuth = m.p.AnthropicAuth
	} else if !m.usesOAuth() && detectedType != "" {
		m.live().anthropicAuth = ""
	}

	m.live().modelPool = []string{}
	m.live().modelPoolFromDiscovery = false
	m.modelAvailability = make(map[string]modelAvailability)
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	if derr == nil && len(discoveredModels) > 0 {
		m.live().modelPool = discoveredModels
		m.live().modelPoolFromDiscovery = true
		m.p.Model = strings.Join(discoveredModels, ",")
		setDebugf("applyModelDetectionResult using discovered model pool count=%d", len(m.live().modelPool))
	}

	if derr != nil {
		m.live().detectionError = derr
		m.focusDetectionAction()
		setDebugf("applyModelDetectionResult detection failed detection_error=%v model_count=%d", m.live().detectionError, len(m.live().modelPool))
		return
	}

	// 本次 set 必须以接口返回的模型为准；不再用旧的本地模型池兜底。
	if len(m.live().modelPool) == 0 {
		m.live().detectionError = fmt.Errorf("%s", locale.T(
			"未从接口获取到任何可用模型，未使用本地旧模型池",
			"no models were fetched from the provider API; local cached models were not used",
		))
		m.focusDetectionAction()
		setDebugf("applyModelDetectionResult no models detection_error=%v", m.live().detectionError)
		return
	}
	if m.usesOAuth() {
		// Reconcile generated mappings against the newly fetched account catalog
		// before filling empty slots. This migrates old Grok defaults such as 4.3
		// and 3-mini to the current 4.6/4.5 catalog without touching user pins.
		provider.ClearUnavailablePreferredDefaults(m.p, m.live().modelPool)
	}

	sort.Strings(m.live().modelPool)
	// Single page: detection success auto-configures the slots and stays on the
	// page. Connection is now the detected endpoint, so it is no longer dirty.
	m.live().connectionDirty = false
	// Record the inputs this successful detection ran against (the raw values
	// before endpoint normalization) as the save baseline. Reverting an edit or
	// cancelling a re-detection naturally returns the page to this state.
	m.live().detectedInputEndpoint = detectedInputEndpoint
	m.live().detectedInputKey = detectedInputKey
	m.applyRecommendation()
	m.cursor = m.mainRowIndex(rowOpus)
	setDebugf(
		"applyModelDetectionResult success provider_type=%q endpoint=%q anthropic_auth=%q model_count=%d stale_slot_count=%d cursor=%d",
		m.p.Type,
		m.p.Endpoint,
		m.p.AnthropicAuth,
		len(m.live().modelPool),
		m.staleSlotCount(),
		m.cursor,
	)
}

// applyRecommendation fills empty slots from the auto recommendation engine and
// records that the config was auto-configured. User-edited fields (identified by
// not matching the recommendation) are left alone.
func (m *AdvancedConfigModel) applyRecommendation() {
	rec := RecommendModels(m.currentWithOneMMarkers(), m.live().modelPool, m.live().modelDisplayMetadata)
	// Only fill slots the user has not already pinned in this session. Detecting
	// again must not overwrite a manual choice made after the first detection.
	if strings.TrimSpace(m.p.OpusModel) == "" {
		m.p.OpusModel = rec.Opus
	}
	if strings.TrimSpace(m.p.SonnetModel) == "" {
		m.p.SonnetModel = rec.Sonnet
	}
	if strings.TrimSpace(m.p.HaikuModel) == "" {
		m.p.HaikuModel = rec.Haiku
	}
	if strings.TrimSpace(m.p.FableModel) == "" {
		m.p.FableModel = rec.Fable
	}
	if strings.TrimSpace(m.p.CustomModelID) == "" {
		m.p.CustomModelID = rec.Custom
	}
	if strings.TrimSpace(m.p.SubagentModel) == "" {
		m.p.SubagentModel = rec.Subagent
	}
	for key, on := range rec.OneMSlots {
		if m.live().oneMSlots[key] == on {
			continue
		}
		m.live().oneMSlots[key] = on
	}
	m.live().autoConfigured = true
	m.live().detectionError = nil
	m.live().connectionDirty = false
	m.live().hadLocalModelPool = countCSV(m.p.Model) > 0
	setDebugf("applyRecommendation slots=%s one_m=%s", slotDebugSummary(*m.p), reviewOneMSummary(m.live().oneMSlots))
}

// handleFetchDone applies a completed connection check. Late results from a
// superseded probe are dropped: the model only accepts the result when the
// probe endpoint/key still match the live inputs.
func (m *AdvancedConfigModel) handleFetchDone(msg modelFetchDoneMsg) {
	if !m.live().detecting || msg.endpoint != m.live().probeEndpoint || msg.apiKey != m.live().probeAPIKey {
		setDebugf(
			"modelFetchDone ignored detecting=%t endpoint_match=%t api_key_match=%t msg_endpoint=%q probe_endpoint=%q",
			m.live().detecting,
			msg.endpoint == m.live().probeEndpoint,
			msg.apiKey == m.live().probeAPIKey,
			msg.endpoint,
			m.live().probeEndpoint,
		)
		return
	}
	m.live().detectProgress = 100
	m.live().detecting = false
	setDebugf(
		"modelFetchDone accepted detected_type=%q detected_endpoint=%q anthropic_auth=%q model_count=%d err=%v",
		msg.detectedType,
		msg.detectedEndpoint,
		msg.anthropicAuth,
		countCSV(msg.discoveredModelsRaw),
		msg.err,
	)
	if msg.contextWindows != nil {
		m.live().modelContextWindows = msg.contextWindows
	}
	previousMetadata := m.live().modelDisplayMetadata
	m.live().modelDisplayMetadata = indexModelInfos(msg.modelInfos)
	if m.usesOAuth() {
		for id, info := range previousMetadata {
			current := m.live().modelDisplayMetadata[id]
			if current.DisplayName == "" {
				current.ID, current.DisplayName = info.ID, info.DisplayName
				m.live().modelDisplayMetadata[id] = current
			}
		}
	}
	for id, window := range contextWindowsFromModelInfos(m.live().modelDisplayMetadata) {
		if _, exists := m.live().modelContextWindows[id]; !exists {
			m.live().modelContextWindows[id] = window
		}
	}
	m.applyModelDetectionResult(msg.detectedType, msg.discoveredModelsRaw, msg.anthropicAuth, msg.detectedEndpoint, msg.err)
	m.markDirty()
}

// handleVerifyDone applies a models.dev key verification result, guarded the
// same way as handleFetchDone.
func (m *AdvancedConfigModel) handleVerifyDone(msg keyVerifyDoneMsg) {
	if !m.usesModelsDev() || !m.live().detecting || msg.endpoint != m.live().probeEndpoint || msg.apiKey != m.live().probeAPIKey || msg.generation != m.live().verifyGeneration {
		setDebugf(
			"keyVerifyDone ignored modelsdev=%t detecting=%t endpoint_match=%t api_key_match=%t",
			m.usesModelsDev(),
			m.live().detecting,
			msg.endpoint == m.live().probeEndpoint,
			msg.apiKey == m.live().probeAPIKey,
		)
		return
	}
	selectedRow := m.currentRow()
	m.live().detectProgress = 100
	m.live().detecting = false
	m.live().detectionError = msg.err
	m.live().keyVerified = msg.err == nil
	// The saved pool is a snapshot; once the connection proves live, refresh it
	// against the current catalog exactly once per verification round.
	if m.live().keyVerified && m.live().modelsDevRefreshPending {
		m.live().modelsDevRefreshPending = false
		catalogID := provider.ModelsDevCatalogID(*m.p)
		setDebugf("models.dev refresh start provider=%q catalog=%q", m.p.Name, catalogID)
		fetchModelsDevRefreshAsync(m.mdDone, catalogID)
	}
	if index := m.mainRowIndex(selectedRow); index >= 0 {
		m.cursor = index
	}
	m.keepCursorVisible()
	setDebugf("keyVerifyDone verified=%t err=%v", m.live().keyVerified, msg.err)
	m.markDirty()
}

func (m *AdvancedConfigModel) handleModelsDevDone(msg modelsDevFetchDoneMsg) {
	// A background refresh for an already-picked provider carries the provider
	// id and must not touch the picker overlay state.
	if msg.refreshFor != "" {
		m.handleModelsDevRefreshDone(msg)
		return
	}
	if !m.modelsDevPicker {
		return
	}
	m.modelsDevLoading = false
	m.modelsDevError = msg.err
	if msg.err == nil {
		m.modelsDevItems = msg.providers
		m.updateModelsDevFilter()
	}
	m.markDirty()
}

// handleModelsDevRefreshDone merges a refreshed catalog into the currently
// edited models.dev provider's pool. The persisted pool is kept as the base:
// user-removed models stay removed when they left the catalog on purpose, but
// models the catalog dropped are gone from the upstream and would only probe
// as unavailable, so they leave the pool along with any slot pointing at them.
// The Custom slot is the exception: it is the user's free-form field, so a
// model pinned there survives even when absent from the catalog (the chat
// fallback still routes it) and stays in the pool. Slot mappings and [1M]
// markers survive: a model present in both lists keeps its slot untouched.
// When the saved pool shares no ID with the catalog at all — a different ID
// namespace rather than a stale list — the merge is purely additive: every
// saved entry is kept and the catalog IDs are appended.
func (m *AdvancedConfigModel) handleModelsDevRefreshDone(msg modelsDevFetchDoneMsg) {
	if !m.usesModelsDev() || m.modelsDevPicker {
		return
	}
	if msg.err != nil {
		setDebugf("models.dev refresh failed provider=%q err=%v; keeping the saved model pool", m.p.Name, msg.err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(msg.refreshFor), provider.ModelsDevCatalogID(*m.p)) {
		setDebugf("models.dev refresh ignored: refreshed=%q current=%q", msg.refreshFor, provider.ModelsDevCatalogID(*m.p))
		return
	}
	var catalogIDs []string
	var catalog modelsdev.Provider
	found := false
	for _, p := range msg.providers {
		if strings.EqualFold(strings.TrimSpace(p.ID), strings.TrimSpace(msg.refreshFor)) {
			catalog = p
			found = true
			break
		}
	}
	if !found {
		// The provider vanished from the catalog entirely. Keep the saved pool
		// rather than wiping a working configuration off the page.
		setDebugf("models.dev refresh: provider %q no longer in catalog; keeping the saved model pool", msg.refreshFor)
		return
	}
	if strings.TrimSpace(m.p.ModelsDevProvider) == "" {
		// Saved before ModelsDevProvider existed: the name was the catalog ID.
		// Record it so a later rename cannot detach the provider from its catalog.
		m.p.ModelsDevProvider = catalog.ID
	}
	catalogIDs = modelsDevCatalogModelIDs(catalog)
	if len(catalogIDs) == 0 {
		setDebugf("models.dev refresh: catalog for %q advertises no routable models; keeping the saved model pool", msg.refreshFor)
		return
	}
	draft, metadata := modelsDevProviderToDraft(catalog)
	catalogSet := make(map[string]bool, len(catalogIDs))
	for _, id := range catalogIDs {
		catalogSet[strings.ToLower(id)] = true
	}
	// A saved pool that shares no ID with the catalog is a different namespace,
	// not a stale list: some gateways serve a broad live /models catalog under
	// their own IDs while models.dev carries a curated subset (ClinePass serves
	// 460 meta-router models, models.dev lists 18 branded ones). Deleting the
	// saved pool there would wipe a working configuration, so the merge turns
	// purely additive: every saved entry is kept and the catalog IDs are
	// appended so they show up in the slot picker.
	additive := false
	savedSet := make(map[string]bool, len(m.live().modelPool))
	for _, id := range m.live().modelPool {
		savedSet[strings.ToLower(strings.TrimSpace(id))] = true
	}
	if len(m.live().modelPool) > 0 {
		overlap := false
		for id := range savedSet {
			if catalogSet[id] {
				overlap = true
				break
			}
		}
		if !overlap {
			additive = true
			setDebugf("models.dev refresh: catalog for %q shares no ID with the saved pool (%d saved, %d catalog); appending catalog IDs",
				msg.refreshFor, len(m.live().modelPool), len(catalogIDs))
		}
	}
	merged := make([]string, 0, len(catalogIDs)+len(m.live().modelPool))
	seen := make(map[string]bool, len(catalogIDs)+len(m.live().modelPool))
	add := func(id string) {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		merged = append(merged, strings.TrimSpace(id))
	}
	// The Custom slot is free-form: a model pinned there survives the merge
	// even when absent from the catalog, and stays in the pool so the slot
	// keeps pointing at a listed entry.
	pinned := make(map[string]bool, 1)
	if custom := strings.ToLower(strings.TrimSpace(m.p.CustomModelID)); custom != "" {
		pinned[custom] = true
	}
	// Saved pool first so user ordering survives the merge. In the additive
	// (different-namespace) case every saved entry is kept verbatim.
	if additive {
		for _, id := range m.live().modelPool {
			add(id)
		}
	} else {
		for _, id := range m.live().modelPool {
			key := strings.ToLower(strings.TrimSpace(id))
			if catalogSet[key] || pinned[key] {
				add(id)
			}
		}
	}
	for _, id := range catalogIDs {
		add(id)
	}
	removed := make([]string, 0, 4)
	if !additive {
		for _, id := range m.live().modelPool {
			key := strings.ToLower(strings.TrimSpace(id))
			if !catalogSet[key] && !pinned[key] {
				removed = append(removed, id)
			}
		}
	}
	if len(removed) == 0 && len(merged) == len(m.live().modelPool) {
		setDebugf("models.dev refresh: catalog unchanged for %q (%d models)", m.p.Name, len(merged))
		return
	}
	m.live().modelPool = merged
	m.p.Model = strings.Join(merged, ",")
	mergedSet := make(map[string]bool, len(merged))
	for _, id := range merged {
		mergedSet[strings.ToLower(strings.TrimSpace(id))] = true
	}
	// The pool gained catalog IDs the saved protocol table may not know (the
	// additive case always does); copy their wire protocols so verification,
	// availability probes, and routing use the catalog's protocol, not the
	// chat fallback.
	if len(draft.ModelProtocols) > 0 {
		if m.p.ModelProtocols == nil {
			m.p.ModelProtocols = make(map[string]string, len(draft.ModelProtocols))
		}
		for id, proto := range draft.ModelProtocols {
			if mergedSet[id] {
				m.p.ModelProtocols[id] = proto
			}
		}
	}
	// Display metadata tracks the catalog: add fresh entries, drop entries for
	// models that left the pool (including the Custom-pinned exception, whose
	// catalog metadata — if any — is gone with the catalog entry).
	for id := range m.live().modelDisplayMetadata {
		if !mergedSet[strings.ToLower(strings.TrimSpace(id))] {
			delete(m.live().modelDisplayMetadata, id)
		}
	}
	for key, info := range metadata {
		if mergedSet[key] {
			m.live().modelDisplayMetadata[key] = info
		}
	}
	for id := range m.live().modelContextWindows {
		if !mergedSet[strings.ToLower(strings.TrimSpace(id))] {
			delete(m.live().modelContextWindows, id)
		}
	}
	for id, window := range contextWindowsFromModelInfos(metadata) {
		if mergedSet[id] {
			if _, exists := m.live().modelContextWindows[id]; !exists {
				m.live().modelContextWindows[id] = window
			}
		}
	}
	// A slot pointing at a model the catalog dropped can no longer route;
	// clear it (and its [1M] marker) instead of leaving a dead mapping. The
	// free-form Custom slot is exempt: mixedProtocolForModel falls back to
	// chat for unknown models, so a pinned value can still route.
	customPinned := strings.ToLower(strings.TrimSpace(m.p.CustomModelID))
	for _, slot := range advancedSlotRefs(m.p) {
		model := strings.TrimSpace(*slot.ptr)
		if model == "" || catalogSet[strings.ToLower(model)] {
			continue
		}
		// Additive (different-namespace) merges keep the saved pool verbatim,
		// so slots pointing into it stay routable and must not be cleared.
		if additive && savedSet[strings.ToLower(model)] {
			continue
		}
		if slot.key == "custom" && strings.ToLower(model) == customPinned {
			continue
		}
		setDebugf("models.dev refresh cleared stale slot=%s model=%q", slot.key, model)
		*slot.ptr = ""
		delete(m.live().oneMSlots, slot.key)
	}
	m.updateFilteredPool()
	setDebugf("models.dev refresh applied provider=%q model_count=%d removed=%d", m.p.Name, len(merged), len(removed))
	m.markDirty()
}

// handleAvailabilityDone applies a finished model availability test. A result
// whose testID no longer matches the current run is dropped.
func (m *AdvancedConfigModel) handleAvailabilityDone(msg modelAvailabilityDoneMsg) {
	if !m.modelTesting || msg.testID != m.modelTestID {
		return
	}
	m.modelTesting = false
	m.modelTestCancel = nil
	m.modelTestCanceled = false
	m.modelAvailability = msg.statuses
	m.live().modelPool = reorderModelsByAvailability(m.live().modelPool, m.modelAvailability)
	m.p.Model = strings.Join(m.live().modelPool, ",")
	m.updateFilteredPool()
	available, unavailable, inconclusive := m.availabilityCounts()
	setDebugf("model availability test finished model_count=%d available=%d unavailable=%d inconclusive=%d", len(m.live().modelPool), available, unavailable, inconclusive)
	m.markDirty()
}

// handleFetchTick advances the connection-check spinner frames while a probe
// is in flight, and clears the copied-value hints two seconds after a copy.
// A single timer watcher drives both cadences.
func (m *AdvancedConfigModel) handleFetchTick() {
	now := time.Now()
	if m.keyCopied && now.Sub(m.lastKeyCopyAt) >= 2*time.Second {
		m.keyCopied = false
	}
	if m.urlCopied && now.Sub(m.lastUrlCopyAt) >= 2*time.Second {
		m.urlCopied = false
	}
	if m.runtimeLoading {
		m.runtimeFrame++
		m.markDirty()
	}
	if m.usesModelsDev() && m.live().autoVerifyPending && now.Sub(m.live().lastKeyEditAt) >= 1200*time.Millisecond {
		m.startModelsDevVerification()
	}
	if !m.live().detecting {
		return
	}
	m.live().detectFrame++
	if m.live().detectProgress < 95 {
		m.live().detectProgress += 3
		if m.live().detectProgress > 95 {
			m.live().detectProgress = 95
		}
	}
	m.markDirty()
}

// handleAvailabilityTick advances the model-test spinner frames while the
// current test run is still in flight.
func (m *AdvancedConfigModel) handleAvailabilityTick() {
	if !m.modelTesting {
		return
	}
	m.modelTestFrame++
	m.markDirty()
}
