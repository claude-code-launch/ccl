package oauthproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiCatalogLimit = 4 << 20

// GeminiModelCatalog is a compatibility fallback, never an account entitlement.
// Keep real wire IDs; public family aliases are derived only from these rows.
func GeminiModelCatalog() []protocol.ModelInfo {
	return []protocol.ModelInfo{
		{ID: "claude-opus-4-6-thinking", DisplayName: "Claude Opus 4.6 Thinking"},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
		{ID: "gemini-3.8-flash-low", DisplayName: "Gemini 3.8 Flash (Low)"},
		{ID: "gemini-3.8-flash-medium", DisplayName: "Gemini 3.8 Flash (Medium)"},
		{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		{ID: "gemini-4-argon-low", DisplayName: "Gemini 4 Argon (Low)"},
		{ID: "gemini-4-argon-medium", DisplayName: "Gemini 4 Argon (Medium)"},
		{ID: "gemini-4-argon-high", DisplayName: "Gemini 4 Argon (High)"},
	}
}

func GeminiModelIDs() []string {
	var ids []string
	for _, model := range GeminiModelCatalog() {
		ids = append(ids, model.ID)
	}
	return ids
}

type geminiCatalog struct {
	// Models retains exact upstream IDs and metadata. Grouped aliases are never
	// written into this cache as if they were upstream model IDs.
	Models       []protocol.ModelInfo `json:"models"`
	DefaultModel string               `json:"default_model,omitempty"`
	BaseURL      string               `json:"base_url,omitempty"`
}

type geminiCatalogCache struct {
	Identity  string        `json:"identity"`
	FetchedAt time.Time     `json:"fetched_at"`
	Catalog   geminiCatalog `json:"catalog"`
}

func geminiCatalogIdentity(path string, credential *antigravityOAuthCredential) string {
	key := filepath.Base(path) + "\x00" + credential.email + "\x00" + credential.projectID + "\x00" + antigravityRequestUserAgent + "\x00" + antigravityBaseURLDaily + "\x00" + antigravityBaseURLProd
	// Imports without email and replacement credentials must not share catalogs.
	if credential.email == "" {
		key += "\x00" + credential.refreshToken
	}
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func (a *antigravityOAuthAuthorizer) discoverModels(ctx context.Context, credential *antigravityOAuthCredential) (*geminiCatalog, bool) {
	identity := geminiCatalogIdentity(a.path, credential)
	cachePath := filepath.Join(filepath.Dir(filepath.Dir(a.path)), "cache", "gemini", identity+".json")
	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	catalog, err := a.fetchModels(discoveryCtx, credential)
	if err == nil {
		raw, marshalErr := json.Marshal(geminiCatalogCache{Identity: identity, FetchedAt: time.Now().UTC(), Catalog: *catalog})
		if marshalErr == nil {
			_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
			if writeErr := fsutil.WriteFileAtomic(cachePath, append(raw, '\n'), 0o600); writeErr != nil {
				LogWarnEvent("model_catalog_cache_failed", "component", "gemini")
			}
		}
		return catalog, false
	}
	// Never log upstream response bodies or credential values here.
	LogWarnEvent("model_catalog_fallback", "component", "gemini", "credential_file", filepath.Base(a.path))
	if file, openErr := os.Open(cachePath); openErr == nil {
		defer file.Close()
		raw, readErr := io.ReadAll(io.LimitReader(file, geminiCatalogLimit+1))
		var cached geminiCatalogCache
		if readErr == nil && len(raw) <= geminiCatalogLimit && json.Unmarshal(raw, &cached) == nil && cached.Identity == identity && validGeminiCatalog(&cached.Catalog) {
			return &cached.Catalog, true
		}
	}
	return nil, true
}

func (a *antigravityOAuthAuthorizer) fetchModels(ctx context.Context, credential *antigravityOAuthCredential) (*geminiCatalog, error) {
	token, err := a.authorizeBound(ctx, false, credential)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: antigravityHTTPTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for refreshed := false; ; refreshed = true {
		// Tier lookup is optional. Bound its share of the discovery deadline so
		// project-only catalog requests can still run during a metadata outage.
		loadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		account, status, _ := geminiLoadCodeAssist(loadCtx, client, token)
		cancel()
		if status != http.StatusUnauthorized || refreshed {
			catalog, status, err := fetchGeminiCatalog(ctx, client, token, credential.projectID, account.catalogTiers(credential.projectID))
			if status != http.StatusUnauthorized || refreshed {
				return catalog, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err = a.authorizeBound(withRejectedOAuthToken(ctx, token), true, credential)
		if err != nil {
			return nil, err
		}
	}
}

func (account geminiCodeAssistAccount) catalogTiers(project string) []string {
	var tiers []string
	if account.project == "" || account.project == project {
		for _, tier := range []string{account.paidTier, account.currentTier} {
			// Never guess a tier or use the allowed/onboarding list as an
			// entitlement. Antigravity sends a project-only request for free.
			if tier != "" && tier != "free-tier" && tier != "legacy-tier" && !slices.Contains(tiers, tier) {
				tiers = append(tiers, tier)
			}
		}
	}
	return append(tiers, "")
}

func fetchGeminiCatalog(ctx context.Context, client *http.Client, token, project string, tiers []string) (*geminiCatalog, int, error) {
	for _, base := range []string{antigravityBaseURLDaily, antigravityBaseURLProd} {
		for _, tier := range tiers {
			body := map[string]any{"project": project}
			if tier != "" {
				body["entitlement"] = map[string]string{"userTier": tier}
			}
			payload, _ := json.Marshal(body)
			raw, status, err := geminiCodeAssistPost(ctx, client, base, "fetchAvailableModels", token, payload, geminiCatalogLimit)
			if status == http.StatusUnauthorized {
				return nil, status, err
			}
			if err == nil {
				catalog, parseErr := parseGeminiCatalog(raw)
				if parseErr == nil {
					catalog.BaseURL = base
					return catalog, status, nil
				}
			}
			switch status {
			case http.StatusBadRequest, http.StatusForbidden, http.StatusPreconditionFailed:
				if tier != "" {
					continue
				}
			}
			break
		}
	}
	return nil, 0, fmt.Errorf("Gemini model discovery failed")
}

func parseGeminiCatalog(raw []byte) (*geminiCatalog, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid Gemini model catalog")
	}
	models := gjson.GetBytes(raw, "models")
	if !models.IsObject() {
		return nil, fmt.Errorf("Gemini catalog has no model inventory")
	}
	catalog := &geminiCatalog{DefaultModel: gjson.GetBytes(raw, "defaultModel").String()}
	models.ForEach(func(key, value gjson.Result) bool {
		id := strings.TrimSpace(key.String())
		if id == "" || strings.ContainsAny(id, "\x00\r\n") || strings.HasPrefix(id, "chat_") || strings.HasPrefix(id, "tab_") || value.Get("disabled").Bool() {
			return true
		}
		info := protocol.ModelInfo{ID: id, DisplayName: value.Get("displayName").String()}
		for _, field := range []string{"maxInputTokens", "contextWindow", "maxTokens"} {
			if n := value.Get(field).Int(); n > 0 {
				info.ContextWindow = int(n)
				break
			}
		}
		for _, field := range []string{"maxOutputTokens", "outputTokenLimit"} {
			if n := value.Get(field).Int(); n > 0 {
				info.MaxOutputTokens = int(n)
				break
			}
		}
		for _, field := range []string{"inputModalities", "supportedInputModalities"} {
			for _, modality := range value.Get(field).Array() {
				m := strings.ToLower(modality.String())
				if m == "text" || m == "image" {
					info.InputModalities = append(info.InputModalities, m)
				}
			}
		}
		if images := value.Get("supportsImages"); len(info.InputModalities) == 0 && (images.Type == gjson.True || images.Type == gjson.False) {
			info.InputModalities = []string{"text"}
			if images.Bool() {
				info.InputModalities = append(info.InputModalities, "image")
			}
		}
		if value.Get("isDefault").Bool() {
			catalog.DefaultModel = id
		}
		catalog.Models = append(catalog.Models, info)
		return len(catalog.Models) <= 4096
	})
	if !validGeminiCatalog(catalog) {
		return nil, fmt.Errorf("empty or invalid Gemini model catalog")
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ID < catalog.Models[j].ID })
	return catalog, nil
}

func validGeminiCatalog(catalog *geminiCatalog) bool {
	if len(catalog.Models) == 0 || len(catalog.Models) > 4096 {
		return false
	}
	seen := make(map[string]bool)
	for _, model := range catalog.Models {
		if model.ID == "" || seen[model.ID] || strings.ContainsAny(model.ID, "\x00\r\n") {
			return false
		}
		seen[model.ID] = true
	}
	return true
}

func geminiModelFamily(id string) (string, string) {
	if strings.HasPrefix(id, "gemini-") {
		for _, level := range []string{"minimal", "low", "medium", "high"} {
			if base, ok := strings.CutSuffix(id, "-"+level); ok {
				return base, level
			}
		}
	}
	return id, ""
}

func stripGeminiDisplayLevel(displayName, level string) string {
	trimmed := strings.TrimSpace(displayName)
	if trimmed == "" || level == "" {
		return trimmed
	}
	suffix := "(" + level + ")"
	if len(trimmed) >= len(suffix) && strings.EqualFold(trimmed[len(trimmed)-len(suffix):], suffix) {
		return strings.TrimSpace(trimmed[:len(trimmed)-len(suffix)])
	}
	return trimmed
}

func (c *geminiCatalog) publicModels() ([]string, map[string]string, map[string]protocol.ModelInfo) {
	var ids []string
	names, details := make(map[string]string), make(map[string]protocol.ModelInfo)
	for _, model := range c.Models {
		base, level := geminiModelFamily(model.ID)
		info, exists := details[base]
		if !exists {
			info = model
			info.ID = base
			info.DisplayName = stripGeminiDisplayLevel(model.DisplayName, level)
			info.ReasoningKnown = strings.HasPrefix(base, "gemini-")
			ids = append(ids, base)
		}
		if level != "" {
			info.ReasoningKnown = true
			info.ReasoningLevels = append(info.ReasoningLevels, protocol.ReasoningLevel{Effort: level, Description: level})
			if model.ID == c.DefaultModel || level == "medium" && info.DefaultReasoningLevel == "" {
				info.DefaultReasoningLevel = level
			}
		}
		if info.ContextWindow == 0 {
			info.ContextWindow = model.ContextWindow
		}
		if info.MaxOutputTokens == 0 {
			info.MaxOutputTokens = model.MaxOutputTokens
		}
		if len(info.InputModalities) == 0 {
			info.InputModalities = append([]string(nil), model.InputModalities...)
		}
		if info.DisplayName == "" {
			info.DisplayName = stripGeminiDisplayLevel(model.DisplayName, level)
		}
		details[base] = info
		names[base] = info.DisplayName
	}
	for id, info := range details {
		sort.SliceStable(info.ReasoningLevels, func(i, j int) bool {
			return geminiEffortRank(info.ReasoningLevels[i].Effort) < geminiEffortRank(info.ReasoningLevels[j].Effort)
		})
		if info.DefaultReasoningLevel == "" && len(info.ReasoningLevels) > 0 {
			info.DefaultReasoningLevel = info.ReasoningLevels[0].Effort
		}
		details[id] = info
	}
	sort.Strings(ids)
	return ids, names, details
}

func (c *geminiCatalog) canonicalModel(model string) string {
	trimmed := stripContextModelSuffix(strings.TrimSpace(model))
	if trimmed == "" || c == nil {
		return trimmed
	}
	for _, info := range c.Models {
		if strings.EqualFold(info.ID, trimmed) {
			return info.ID
		}
	}
	ids, names, _ := c.publicModels()
	for _, id := range ids {
		if strings.EqualFold(id, trimmed) {
			return id
		}
	}
	for _, id := range ids {
		if strings.EqualFold(strings.TrimSpace(names[id]), trimmed) {
			return id
		}
	}
	for _, info := range c.Models {
		if strings.EqualFold(strings.TrimSpace(info.DisplayName), trimmed) {
			return info.ID
		}
	}
	return trimmed
}

func geminiEffortRank(effort string) int {
	switch effort {
	case "minimal":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}

func applyGeminiThinkingConfig(body []byte, level string, maxOutputTokens int) []byte {
	if level == "" {
		return body
	}
	body, _ = sjson.DeleteBytes(body, "generationConfig.thinkingConfig.thinkingBudget")
	body, _ = sjson.SetBytes(body, "generationConfig.thinkingConfig.thinkingLevel", level)
	body, _ = sjson.SetBytes(body, "generationConfig.thinkingConfig.includeThoughts", true)
	// Gemini maxOutputTokens caps thoughtsTokenCount + candidatesTokenCount.
	// Claude Code sends small fallback max_tokens for non-Claude model IDs,
	// which can be exhausted entirely inside a high-effort thinking block.
	if gjson.GetBytes(body, "generationConfig.maxOutputTokens").Exists() {
		minMaxTokens := maxOutputTokens
		if minMaxTokens <= 0 {
			minMaxTokens = 65_536
		}
		if int(gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int()) < minMaxTokens {
			body, _ = sjson.SetBytes(body, "generationConfig.maxOutputTokens", minMaxTokens)
		}
	}
	return body
}

func (c *geminiCatalog) resolve(model, effort string, body []byte) (string, []byte, error) {
	resolved := c.canonicalModel(model)
	_, _, details := c.publicModels()
	// An explicit wire ID wins over a generic client effort setting.
	for _, info := range c.Models {
		if info.ID == resolved {
			base, level := geminiModelFamily(resolved)
			if level == "" && effort != "" {
				hasVariants := false
				for _, candidate := range c.Models {
					candBase, variant := geminiModelFamily(candidate.ID)
					if candBase == resolved && variant != "" {
						hasVariants = true
						break
					}
				}
				if hasVariants {
					break
				}
			}
			maxOut := info.MaxOutputTokens
			if maxOut <= 0 {
				maxOut = details[base].MaxOutputTokens
			}
			body = applyGeminiThinkingConfig(body, level, maxOut)
			return resolved, body, nil
		}
	}
	info, ok := details[resolved]
	if !ok {
		return "", nil, fmt.Errorf("model %q is not in the bound Gemini account catalog", model)
	}
	if effort == "" {
		effort = info.DefaultReasoningLevel
	} else {
		effort = geminiThinkingLevel(effort)
	}
	for _, candidate := range c.Models {
		base, level := geminiModelFamily(candidate.ID)
		if base == resolved && level == effort {
			maxOut := candidate.MaxOutputTokens
			if maxOut <= 0 {
				maxOut = info.MaxOutputTokens
			}
			body = applyGeminiThinkingConfig(body, level, maxOut)
			return candidate.ID, body, nil
		}
	}
	return "", nil, fmt.Errorf("thinking level %q is not available for Gemini model %q", effort, resolved)
}
