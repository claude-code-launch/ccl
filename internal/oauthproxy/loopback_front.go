package oauthproxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/protocol"
)

// loopbackFrontMux serves Responses and adapts Chat Completions to it.
func loopbackFrontMux(key string, models, responses http.HandlerFunc) *http.ServeMux {
	statefulResponses := withResponsesHistory(key, responses)
	chat := chatCompletionsFront(key, responses)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	for _, prefix := range []string{"/v1", ""} {
		mux.HandleFunc(prefix+"/models", models)
		mux.HandleFunc(prefix+"/responses", statefulResponses)
		mux.HandleFunc(prefix+"/responses/compact", responses)
		mux.HandleFunc(prefix+"/chat/completions", chat)
	}
	return mux
}

// loopbackAuthorized checks the per-session key a client sends as a bearer
// token (or x-api-key). The comparison is constant-time.
func loopbackAuthorized(request *http.Request, key string) bool {
	if key == "" {
		return false
	}
	if keyMatches(request.Header.Get("x-api-key"), key) {
		return true
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	return keyMatches(bearer, key)
}

func keyMatches(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// modelsRequestAllowed rejects an unauthorized or non-GET model-list request,
// writing the error. It reports whether the handler may proceed.
func modelsRequestAllowed(writer http.ResponseWriter, request *http.Request, key string) bool {
	if !loopbackAuthorized(request, key) {
		writeProxyError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return false
	}
	if request.Method != http.MethodGet {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return false
	}
	return true
}

// inferenceRequestAllowed applies the same checks to a POST inference request.
func inferenceRequestAllowed(writer http.ResponseWriter, request *http.Request, key string) bool {
	if !loopbackAuthorized(request, key) {
		writeProxyError(writer, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return false
	}
	if request.Method != http.MethodPost {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return false
	}
	return true
}

// modelListEntry is one row of the OpenAI-shaped model list. api_backend tells
// clients that support several wire protocols (such as Grok Build) to use
// Responses, the protocol every CCL runtime implements without translation on
// the client side.
func modelListEntry(model string) map[string]any {
	return map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "ccl", "api_backend": "responses"}
}

// serveModelList answers a model-list request with plain IDs.
func serveModelList(writer http.ResponseWriter, request *http.Request, key string, models []string) {
	serveModelListWithDetails(writer, request, key, models, nil)
}

// serveModelListWithDetails answers a model-list request enriched with known
// per-model metadata (context_window, max_output_tokens, input_modalities,
// reasoning_efforts) so Responses clients like Grok Build and Codex see
// accurate limits, effort controls, and backends.
func serveModelListWithDetails(writer http.ResponseWriter, request *http.Request, key string, models []string, details map[string]protocol.ModelInfo) {
	serveModelListWithRoutes(writer, request, key, models, details, nil)
}

func serveModelListWithRoutes(writer http.ResponseWriter, request *http.Request, key string, models []string, details map[string]protocol.ModelInfo, routes map[string]string) {
	if !modelsRequestAllowed(writer, request, key) {
		return
	}
	index, _ := modelsdev.CachedIndex()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entry := modelListEntry(model)
		info, hasDetail := lookupModelDetail(details, model)
		target := strings.TrimSpace(model)
		if routes != nil {
			if routed := strings.TrimSpace(routes[strings.ToLower(target)]); routed != "" {
				target = routed
			}
		}
		displayName := strings.TrimSpace(info.DisplayName)
		if hasDetail {
			if displayName != "" && !strings.EqualFold(displayName, model) {
				entry["display_name"] = displayName
			}
			if info.ContextWindow > 0 {
				entry["context_window"] = info.ContextWindow
				entry["max_input_tokens"] = info.ContextWindow
			}
			if info.MaxOutputTokens > 0 {
				entry["max_output_tokens"] = info.MaxOutputTokens
			}
			if len(info.InputModalities) > 0 {
				entry["input_modalities"] = append([]string(nil), info.InputModalities...)
			}
		}
		if index != nil {
			if _, ok := entry["input_modalities"]; !ok {
				if mod, found := index.InputModalities("", target, firstNonEmptyString(displayName, model)); found && len(mod) > 0 {
					entry["input_modalities"] = mod
				}
			}
			if _, ok := entry["max_output_tokens"]; !ok {
				if maxOut, found := index.MaxOutputTokens("", target, firstNonEmptyString(displayName, model)); found && maxOut > 0 {
					entry["max_output_tokens"] = maxOut
				}
			}
		}
		levels, defaultLevel, known := info.ReasoningLevels, info.DefaultReasoningLevel, info.ReasoningKnown || len(info.ReasoningLevels) > 0
		if !known {
			levels, defaultLevel, known = resolveFallbackModelReasoning(index, "", target, firstNonEmptyString(displayName, model))
		}
		applyModelEntryReasoning(entry, levels, defaultLevel, known)
		data = append(data, entry)
	}
	writeModelList(writer, data)
}

func resolveFallbackModelReasoning(index *modelsdev.Index, providerID, target, displayOrAlias string) ([]protocol.ReasoningLevel, string, bool) {
	bare := strings.TrimSpace(stripContextModelSuffix(target))
	lower := strings.ToLower(bare)
	if _, level := geminiModelFamily(lower); level != "" {
		return nil, "", true
	}
	if lower == "kimi-for-coding-highspeed" {
		return nil, "", true
	}
	if index != nil {
		candidates := []string{bare}
		if trimmed := strings.TrimSuffix(lower, "-thinking"); trimmed != "" && !strings.EqualFold(trimmed, bare) {
			candidates = append(candidates, trimmed)
		}
		for _, candidate := range candidates {
			if efforts, found := index.Efforts(providerID, candidate, displayOrAlias); found {
				levels, chosen := reasoningLevelsFromEfforts(efforts, "")
				return levels, chosen, true
			}
		}
	}
	if lower == "kimi-for-coding" {
		levels, chosen := reasoningLevelsFromEfforts([]string{"low", "high", "max"}, "high")
		return levels, chosen, true
	}
	return protocol.GPTReasoningLevels(), protocol.GPTDefaultReasoningLevel, true
}

func reasoningLevelsFromEfforts(efforts []string, preferredDefault string) ([]protocol.ReasoningLevel, string) {
	if len(efforts) == 0 {
		return nil, ""
	}
	levels := make([]protocol.ReasoningLevel, 0, len(efforts))
	seen := make(map[string]bool, len(efforts))
	for _, raw := range efforts {
		effort := strings.ToLower(strings.TrimSpace(raw))
		if effort == "" || effort == "none" || seen[effort] {
			continue
		}
		seen[effort] = true
		_, desc, _ := grokEffortOptionMeta(effort, "")
		if desc == "" {
			desc = effort
		}
		levels = append(levels, protocol.ReasoningLevel{Effort: effort, Description: desc})
	}
	if len(levels) == 0 {
		return nil, ""
	}
	return levels, chooseDefaultEffort(levels, preferredDefault)
}

func chooseDefaultEffort(levels []protocol.ReasoningLevel, preferred string) string {
	if len(levels) == 0 {
		return ""
	}
	want := strings.ToLower(strings.TrimSpace(preferred))
	if want != "" {
		for _, level := range levels {
			if strings.EqualFold(level.Effort, want) {
				return strings.ToLower(strings.TrimSpace(level.Effort))
			}
		}
	}
	for _, candidate := range []string{"medium", "high", "low"} {
		for _, level := range levels {
			if strings.EqualFold(level.Effort, candidate) {
				return candidate
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(levels[0].Effort))
}

// applyModelEntryReasoning writes both Grok Build's reasoning-effort schema
// (supports_reasoning_effort, reasoning_efforts, reasoning_effort_server_default)
// and Codex/OpenAI's schema (supported_reasoning_levels, default_reasoning_level).
// Grok Build's ReasoningEffort enum only accepts none|minimal|low|medium|high|xhigh|max,
// so rungs outside that set (such as Codex's "ultra") are omitted from reasoning_efforts.
func partitionReasoningLevels(levels []protocol.ReasoningLevel) (codexLevels, grokLevels []protocol.ReasoningLevel) {
	codexLevels = make([]protocol.ReasoningLevel, 0, len(levels))
	seenCodex := make(map[string]bool, len(levels))
	grokLevels = make([]protocol.ReasoningLevel, 0, len(levels))
	for _, level := range levels {
		effort := strings.ToLower(strings.TrimSpace(level.Effort))
		if effort == "" || effort == "none" || seenCodex[effort] {
			continue
		}
		seenCodex[effort] = true
		_, desc, grokValid := grokEffortOptionMeta(effort, level.Description)
		if desc == "" {
			desc = firstNonEmptyString(level.Description, effort)
		}
		normalized := protocol.ReasoningLevel{Effort: effort, Description: desc}
		codexLevels = append(codexLevels, normalized)
		if grokValid {
			grokLevels = append(grokLevels, normalized)
		}
	}
	return codexLevels, grokLevels
}

func chooseGrokDefaultEffort(grokLevels []protocol.ReasoningLevel, defaultLevel string) string {
	preferred := strings.ToLower(strings.TrimSpace(defaultLevel))
	if preferred == "ultra" {
		preferred = "max"
	}
	return chooseDefaultEffort(grokLevels, preferred)
}

func applyModelEntryReasoning(entry map[string]any, levels []protocol.ReasoningLevel, defaultLevel string, known bool) {
	if entry == nil {
		return
	}
	codexLevels, grokLevels := partitionReasoningLevels(levels)
	if len(codexLevels) > 0 {
		entry["supported_reasoning_levels"] = codexLevels
		if chosen := chooseDefaultEffort(codexLevels, defaultLevel); chosen != "" {
			entry["default_reasoning_level"] = chosen
		}
	}
	if len(grokLevels) > 0 {
		grokDefault := chooseGrokDefaultEffort(grokLevels, defaultLevel)
		options := make([]map[string]any, 0, len(grokLevels))
		for _, level := range grokLevels {
			label, desc, _ := grokEffortOptionMeta(level.Effort, level.Description)
			opt := map[string]any{
				"value":       level.Effort,
				"label":       label,
				"description": desc,
			}
			if level.Effort == grokDefault {
				opt["default"] = true
			}
			options = append(options, opt)
		}
		entry["supports_reasoning_effort"] = true
		entry["reasoning_efforts"] = options
		if grokDefault != "" {
			entry["reasoning_effort_server_default"] = grokDefault
		}
		return
	}
	if known {
		entry["supports_reasoning_effort"] = false
	}
}

func grokEffortOptionMeta(effort, customDesc string) (label, description string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		label, description = "Minimal Effort", "Minimal reasoning for fast responses"
	case "low":
		label, description = "Low Effort", "Fast responses with lighter reasoning"
	case "medium":
		label, description = "Medium Effort", "Balances speed and reasoning depth for everyday tasks"
	case "high":
		label, description = "High Effort", "Greater reasoning depth for complex problems"
	case "xhigh":
		label, description = "Extra High Effort", "Extra high reasoning depth for complex problems"
	case "max":
		label, description = "Max Effort", "Maximum reasoning depth for the hardest problems"
	case "ultra":
		return "", firstNonEmptyString(customDesc, "Maximum reasoning with automatic task delegation"), false
	default:
		return "", strings.TrimSpace(customDesc), false
	}
	if trimmed := strings.TrimSpace(customDesc); trimmed != "" && !strings.EqualFold(trimmed, effort) {
		description = trimmed
	}
	return label, description, true
}

func lookupModelDetail(details map[string]protocol.ModelInfo, model string) (protocol.ModelInfo, bool) {
	if len(details) == 0 {
		return protocol.ModelInfo{}, false
	}
	trimmed := strings.TrimSpace(model)
	candidates := []string{trimmed, strings.ToLower(trimmed)}
	if bare := stripContextModelSuffix(trimmed); bare != trimmed {
		candidates = append(candidates, bare, strings.ToLower(bare))
	}
	for _, key := range candidates {
		if info, ok := details[key]; ok {
			return info, true
		}
	}
	bare := stripContextModelSuffix(trimmed)
	for key, info := range details {
		k := strings.TrimSpace(key)
		if strings.EqualFold(k, trimmed) || (bare != trimmed && strings.EqualFold(k, bare)) {
			return info, true
		}
	}
	return protocol.ModelInfo{}, false
}

func writeModelList(writer http.ResponseWriter, data []map[string]any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
}

// writeProxyError writes an OpenAI-shaped error body.
func writeProxyError(writer http.ResponseWriter, status int, errorType, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errorType, "code": nil, "param": nil},
	})
}

// prepareEventStream sets the headers of a server-sent event response.
func prepareEventStream(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
}
