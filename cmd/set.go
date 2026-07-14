package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	// 🔥 引入 ccl 统一的国际化组件
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"

	// 统一使用指定的私有域 v2 包
	tea "charm.land/bubbletea/v2"
)

var setCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Add or update an LLM provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunProviderSet(args)
	},
}

func RunProviderSet(args []string) error {
	setDebugf("RunProviderSet start args=%q", strings.Join(args, " "))
	cfg, err := config.Load()
	if err != nil {
		setDebugf("config load failed err=%v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider.Provider)
	}

	var targetName string
	if len(args) > 0 {
		targetName = strings.TrimSpace(args[0])
	} else {
		// 没有传 provider name：显示已有 provider 列表，提供新建入口
		var names []string
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)

		if len(names) > 0 {
			var providerItems []string
			providerItems = append(providerItems, locale.T("+ 新建 Provider", "+ Create new provider"))
			for _, name := range names {
				label := name
				if name == cfg.ActiveProvider {
					label = fmt.Sprintf("%s %s", name, locale.T("(当前使用)", "(active)"))
				}
				providerItems = append(providerItems, label)
			}

			chosen, err := runSelect(locale.T("选择 Provider 或新建:", "Select a provider or create new:"), providerItems)
			if err != nil {
				return err
			}
			if chosen == "" {
				return nil
			}

			// First item is "Create new"
			if chosen == providerItems[0] {
				targetName = ""
			} else {
				// Match label back to original name
				for _, name := range names {
					label := name
					if name == cfg.ActiveProvider {
						label = fmt.Sprintf("%s %s", name, locale.T("(当前使用)", "(active)"))
					}
					if chosen == label {
						targetName = name
						break
					}
				}
			}
		}
	}

	var p provider.Provider
	isUpdate := false
	if targetName != "" {
		if existing, exists := cfg.Providers[targetName]; exists {
			p = existing
			isUpdate = true
		} else {
			p.Name = targetName
		}
	}
	if targetName == "" {
		targetName = "default"
		p.Name = targetName
	}

	// 🚀 运行基于特定域 v2 架构的超级大面板
	m := NewAdvancedConfigModel(&p)
	if p.OAuthProvider != "" {
		runtimeProvider, cleanup, err := prepareProviderRuntime(p)
		if err != nil {
			return fmt.Errorf("prepare OAuth provider for configuration: %w", err)
		}
		defer cleanup()
		m.configureOAuthRuntime(runtimeProvider.Endpoint, runtimeProvider.APIKey)
	}
	program := tea.NewProgram(m)
	finalModel, err := program.Run()
	if err != nil {
		setDebugf("advanced config panel failed err=%v", err)
		return fmt.Errorf("failed running advanced config panel: %w", err)
	}

	updatedModel := finalModel.(*AdvancedConfigModel)
	p = *updatedModel.p
	setDebugf(
		"advanced config finished name=%q endpoint_empty=%t api_key_len=%d type=%q model_count=%d effort=%q slots=%s one_m=%s page=%d cursor=%d detecting=%t detection_error=%v model_pool_count=%d",
		p.Name,
		p.Endpoint == "",
		len(p.APIKey),
		p.Type,
		countCSV(p.Model),
		p.EffortLevel,
		slotDebugSummary(p),
		reviewOneMSummary(updatedModel.oneMSlots),
		updatedModel.page,
		updatedModel.cursor,
		updatedModel.detecting,
		updatedModel.detectionError,
		len(updatedModel.modelPool),
	)
	if !updatedModel.saveConfirmed {
		setDebugf("abort: save was not confirmed")
		fmt.Fprintln(os.Stderr, locale.T("ℹ️ 已取消配置，未保存。", "ℹ️ Configuration canceled; no changes were saved."))
		return nil
	}

	// 协议探测/模型获取失败 → 直接退出，不保存
	if updatedModel.detectionError != nil {
		setDebugf("abort: detection error err=%v", updatedModel.detectionError)
		fmt.Fprintf(os.Stderr, "❌ %s\n   %v\n",
			locale.T("协议探测与模型获取均失败，已退出配置", "protocol detection and model fetching both failed; aborted"),
			updatedModel.detectionError)
		return updatedModel.detectionError
	}

	// 未完成探测就退出（如在凭据页按 Esc）→ 不保存半成品配置
	if !providerConfigurationComplete(p) {
		setDebugf(
			"abort: incomplete config endpoint_empty=%t api_key_empty=%t type_empty=%t model_empty=%t type=%q model_count=%d effort=%q slots=%s",
			p.Endpoint == "",
			p.APIKey == "",
			p.Type == "",
			p.Model == "",
			p.Type,
			countCSV(p.Model),
			p.EffortLevel,
			slotDebugSummary(p),
		)
		fmt.Fprintln(os.Stderr, locale.T("ℹ️ 配置未完成，已退出，未保存。", "ℹ️ Configuration incomplete; aborted without saving."))
		return nil
	}

	applyCompactConfig(&p, updatedModel.oneMSlots, updatedModel.compactPreset)
	setDebugf(
		"after applyCompactConfig provider=%q type=%q effort=%q model_count=%d slots=%s compact=%s active_chosen=%t",
		p.Name,
		p.Type,
		p.EffortLevel,
		countCSV(p.Model),
		slotDebugSummary(p),
		updatedModel.compactSummary(),
		updatedModel.IsActiveChosen,
	)

	cfg.Providers[p.Name] = p
	if updatedModel.IsActiveChosen {
		cfg.ActiveProvider = p.Name
	}
	if err := config.Save(cfg); err != nil {
		setDebugf("config save failed err=%v", err)
		return fmt.Errorf("failed to save config: %w", err)
	}
	setDebugf("config saved provider=%q active=%t", p.Name, updatedModel.IsActiveChosen)

	fmt.Println("")
	if isUpdate {
		fmt.Printf("✅ %s %q\n", locale.T("已更新 Provider", "Successfully updated provider"), p.Name)
	} else {
		fmt.Printf("✅ %s %q\n", locale.T("已添加 Provider", "Successfully added provider"), p.Name)
	}
	return nil
}

func providerConfigurationComplete(p provider.Provider) bool {
	if strings.TrimSpace(p.Type) == "" || strings.TrimSpace(p.Model) == "" {
		return false
	}
	if strings.TrimSpace(p.OAuthProvider) != "" {
		return true
	}
	return strings.TrimSpace(p.Endpoint) != "" && strings.TrimSpace(p.APIKey) != ""
}

func init() {
	rootCmd.AddCommand(setCmd)
}

func SetCMD() *cobra.Command {
	return setCmd
}

func setDebugf(format string, args ...any) {
	if os.Getenv("CCL_SET_DEBUG") == "" && os.Getenv("CCL_DEBUG") == "" {
		return
	}
	path := os.Getenv("CCL_SET_DEBUG_LOG")
	if path == "" {
		path = os.Getenv("CCL_DEBUG_LOG")
	}
	if path == "" {
		path = "/tmp/ccl-set-debug.log"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s ", time.Now().Format(time.RFC3339Nano))
	_, _ = fmt.Fprintf(f, format, args...)
	_, _ = fmt.Fprintln(f)
}

func countCSV(csv string) int {
	count := 0
	for _, item := range strings.Split(csv, ",") {
		if strings.TrimSpace(item) != "" {
			count++
		}
	}
	return count
}

func slotDebugSummary(p provider.Provider) string {
	return fmt.Sprintf(
		"opus_set=%t sonnet_set=%t haiku_set=%t custom_set=%t subagent_set=%t",
		strings.TrimSpace(p.OpusModel) != "",
		strings.TrimSpace(p.SonnetModel) != "",
		strings.TrimSpace(p.HaikuModel) != "",
		strings.TrimSpace(p.CustomModelID) != "",
		strings.TrimSpace(p.SubagentModel) != "",
	)
}

func stringInSlice(s string, slice []string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// detectProtocolAndModels probes only the /models endpoint derived from the
// exact user-supplied base URL. Paths ending in /vN or /codex prefer OpenAI
// Bearer auth; unversioned paths ending in /claude or /anthropic prefer native
// Anthropic x-api-key auth. The returned model-list shape takes precedence over
// those path hints.
//
// OpenAI Chat and Responses share the same model-list shape, so automatic
// detection intentionally stops at the OpenAI family. The user chooses the
// concrete OpenAI protocol on the Review page without an extra paid request.
// Returns (protocol, comma-separated-models, error).
// error is non-nil when protocol detection fails.
func detectProtocolAndModels(endpoint, apiKey string) (string, string, error) {
	result := detectProtocolAndModelsDetailed(endpoint, apiKey)
	return result.protocol, result.models, result.err
}

type protocolDetectionResult struct {
	protocol      string
	models        string
	baseURL       string
	anthropicAuth string
	err           error
}

const (
	anthropicAuthXAPIKey = "x-api-key"
	anthropicAuthBearer  = "bearer"
)

func detectProtocolAndModelsDetailed(endpoint, apiKey string) protocolDetectionResult {
	endpoint = strings.TrimSuffix(endpoint, "/")
	setDebugf("detectProtocolAndModelsDetailed start endpoint=%q api_key_len=%d", endpoint, len(apiKey))
	if suggestion, invalid := protocol.InvalidCodexV1EndpointSuggestion(endpoint); invalid {
		return protocolDetectionResult{err: fmt.Errorf("%s", locale.T(
			fmt.Sprintf("Codex endpoint 应填写为 %s，不要包含 /v1；获取模型时 ccl 会请求 %s/models", suggestion, suggestion),
			fmt.Sprintf("Codex endpoint must be %s without /v1; ccl will request %s/models for model discovery", suggestion, suggestion),
		))}
	}

	var failures []modelProbeFailure
	for _, candidate := range buildModelProbeCandidates(endpoint) {
		setDebugf("detect probe start name=%s auth=%s url=%q corrected_base=%q", candidate.name, candidate.auth, candidate.modelsURL, candidate.baseURL)
		result, err := fetchCandidateModelsForDetection(candidate, apiKey)
		if err != nil {
			setDebugf("detect probe failed name=%s err=%v", candidate.name, err)
			failures = append(failures, modelProbeFailure{candidate: candidate, err: err})
			continue
		}
		if result.models == "" {
			err := fmt.Errorf("empty model list shape=%q", result.shape)
			setDebugf("detect probe empty name=%s shape=%q", candidate.name, result.shape)
			failures = append(failures, modelProbeFailure{candidate: candidate, err: err})
			continue
		}
		if detection, ok := classifyModelProbeResult(candidate, result); ok {
			return detection
		}
		err = fmt.Errorf("unexpected model list shape %q for %s", result.shape, candidate.expect)
		setDebugf("detect probe rejected name=%s shape=%q expect=%s", candidate.name, result.shape, candidate.expect)
		failures = append(failures, modelProbeFailure{candidate: candidate, err: err})
	}
	return unsupportedProtocolResult(failures)
}

func unsupportedProtocolResult(failures []modelProbeFailure) protocolDetectionResult {
	reason := summarizeProbeFailures(failures)
	if reason != "" {
		setDebugf("detect failed summary=%q", reason)
		return protocolDetectionResult{err: fmt.Errorf("%s: %s", locale.T(
			"暂不支持这个协议：Anthropic 与 OpenAI 模型列表均获取失败",
			"unsupported protocol: both Anthropic and OpenAI model-list probes failed",
		), reason)}
	}
	return protocolDetectionResult{err: fmt.Errorf("%s", locale.T(
		"暂不支持这个协议：Anthropic 与 OpenAI 模型列表均获取失败",
		"unsupported protocol: both Anthropic and OpenAI model-list probes failed",
	))}
}

type modelListShape string

const (
	modelListShapeUnknown   modelListShape = ""
	modelListShapeOpenAI    modelListShape = "openai"
	modelListShapeAnthropic modelListShape = "anthropic"
)

type modelListDetection struct {
	models string
	shape  modelListShape
}

type modelProbeAuth string

const (
	modelProbeAuthBearer  modelProbeAuth = "bearer"
	modelProbeAuthXAPIKey modelProbeAuth = "x-api-key"
)

type modelProbeExpectation string

const (
	modelProbeExpectOpenAI    modelProbeExpectation = "openai-compatible"
	modelProbeExpectAnthropic modelProbeExpectation = "anthropic-native"
)

type modelProbeCandidate struct {
	name      string
	modelsURL string
	baseURL   string
	auth      modelProbeAuth
	expect    modelProbeExpectation
}

type modelProbeFailure struct {
	candidate modelProbeCandidate
	err       error
}

func buildModelProbeCandidates(endpoint string) []modelProbeCandidate {
	baseURL := normalizeModelBaseURL(endpoint)
	if endpointLikelyAnthropic(baseURL) {
		modelsURL := protocol.NormalizeAnthropicModelsURL(baseURL)
		return []modelProbeCandidate{
			{name: "anthropic-path-x-api-key", modelsURL: modelsURL, baseURL: baseURL, auth: modelProbeAuthXAPIKey, expect: modelProbeExpectAnthropic},
			{name: "anthropic-path-bearer", modelsURL: modelsURL, baseURL: baseURL, auth: modelProbeAuthBearer, expect: modelProbeExpectAnthropic},
		}
	}
	modelsURL := protocol.NormalizeOpenAIModelsURL(baseURL)
	return []modelProbeCandidate{
		{name: "openai-path-bearer", modelsURL: modelsURL, baseURL: baseURL, auth: modelProbeAuthBearer, expect: modelProbeExpectOpenAI},
		{name: "openai-path-x-api-key", modelsURL: modelsURL, baseURL: baseURL, auth: modelProbeAuthXAPIKey, expect: modelProbeExpectAnthropic},
	}
}

func classifyModelProbeResult(candidate modelProbeCandidate, result modelListDetection) (protocolDetectionResult, bool) {
	if result.shape == modelListShapeAnthropic {
		auth := anthropicAuthXAPIKey
		if candidate.auth == modelProbeAuthBearer {
			auth = anthropicAuthBearer
		}
		setDebugf("detect selected anthropic auth=%s model_count=%d shape=%q probe=%s base_url=%q", auth, countCSV(result.models), result.shape, candidate.name, candidate.baseURL)
		return protocolDetectionResult{protocol: "anthropic", models: result.models, baseURL: candidate.baseURL, anthropicAuth: auth}, true
	}

	if result.shape == modelListShapeOpenAI || candidate.expect == modelProbeExpectOpenAI {
		setDebugf("detect selected openai family model_count=%d shape=%q probe=%s base_url=%q", countCSV(result.models), result.shape, candidate.name, candidate.baseURL)
		return protocolDetectionResult{protocol: "openai", models: result.models, baseURL: candidate.baseURL}, true
	}
	if result.shape == modelListShapeUnknown && candidate.expect == modelProbeExpectAnthropic {
		auth := anthropicAuthXAPIKey
		if candidate.auth == modelProbeAuthBearer {
			auth = anthropicAuthBearer
		}
		return protocolDetectionResult{protocol: "anthropic", models: result.models, baseURL: candidate.baseURL, anthropicAuth: auth}, true
	}

	return protocolDetectionResult{}, false
}

func fetchCandidateModelsForDetection(candidate modelProbeCandidate, apiKey string) (modelListDetection, error) {
	if strings.TrimSpace(apiKey) == "" {
		return modelListDetection{}, fmt.Errorf("api key 不能为空")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, candidate.modelsURL, nil)
	if err != nil {
		return modelListDetection{}, err
	}
	switch candidate.auth {
	case modelProbeAuthBearer:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case modelProbeAuthXAPIKey:
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
	}
	return fetchModelListForDetection(req, 8*time.Second)
}

func endpointHasVersionSuffix(endpoint string) bool {
	return endpointVersionSuffix(endpoint) != ""
}

func endpointLikelyAnthropic(endpoint string) bool {
	if endpointHasVersionSuffix(endpoint) {
		return false
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	last := strings.ToLower(parts[len(parts)-1])
	return last == "claude" || last == "anthropic"
}

func endpointVersionSuffix(endpoint string) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil {
		return ""
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) > 0 {
		last := strings.ToLower(parts[len(parts)-1])
		if last == "models" || last == "messages" {
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) >= 2 && strings.EqualFold(parts[len(parts)-2], "chat") && strings.EqualFold(parts[len(parts)-1], "completions") {
		parts = parts[:len(parts)-2]
	}
	if len(parts) == 0 {
		return ""
	}
	last := strings.ToLower(parts[len(parts)-1])
	if len(last) < 2 || last[0] != 'v' {
		return ""
	}
	for _, r := range last[1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return last
}

func normalizeModelBaseURL(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case strings.HasSuffix(endpoint, "/models"):
		return strings.TrimSuffix(endpoint, "/models")
	case strings.HasSuffix(endpoint, "/messages"):
		return strings.TrimSuffix(endpoint, "/messages")
	default:
		return endpoint
	}
}

type modelProbeHTTPError struct {
	statusCode int
	status     string
	preview    string
}

func (e modelProbeHTTPError) Error() string {
	if e.preview == "" {
		return e.status
	}
	return fmt.Sprintf("%s body=%q", e.status, e.preview)
}

func summarizeProbeFailures(failures []modelProbeFailure) string {
	if len(failures) == 0 {
		return ""
	}

	hasUnauthorized := false
	hasNotFound := false
	hasMethodNotAllowed := false
	hasNetwork := false
	hasHTML := false
	for _, failure := range failures {
		var httpErr modelProbeHTTPError
		if errors.As(failure.err, &httpErr) {
			switch httpErr.statusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				hasUnauthorized = true
			case http.StatusNotFound:
				hasNotFound = true
			case http.StatusMethodNotAllowed:
				hasMethodNotAllowed = true
			}
			if strings.Contains(strings.ToLower(httpErr.preview), "<html") {
				hasHTML = true
			}
			continue
		}
		if errors.Is(failure.err, context.DeadlineExceeded) {
			hasNetwork = true
			continue
		}
		var netErr interface{ Timeout() bool }
		if errors.As(failure.err, &netErr) && netErr.Timeout() {
			hasNetwork = true
		}
	}

	switch {
	case hasUnauthorized:
		return locale.T("地址可访问，但 API Key 或鉴权方式不正确", "endpoint is reachable, but API key or auth type is invalid")
	case hasNotFound:
		return locale.T("模型列表路径不存在，请检查 endpoint 或确认服务商支持 /models", "model-list path was not found; check the endpoint or confirm the provider supports /models")
	case hasMethodNotAllowed:
		return locale.T("模型列表 endpoint 存在但不接受 GET 方法", "model-list endpoint exists but does not accept GET")
	case hasHTML:
		return locale.T("返回的是 HTML，可能填成了网页地址而不是 API 地址", "response was HTML; the URL may be a website URL rather than an API base URL")
	case hasNetwork:
		return locale.T("网络连接失败或请求超时", "network connection failed or timed out")
	default:
		return locale.T("返回体无法识别为 OpenAI 或 Anthropic 模型列表", "response body was not recognized as an OpenAI or Anthropic model list")
	}
}

func fetchModelListForDetection(req *http.Request, timeout time.Duration) (modelListDetection, error) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		setDebugf("models probe transport error url=%q err=%v", req.URL.String(), err)
		return modelListDetection{}, err
	}
	defer resp.Body.Close()
	setDebugf("models probe status url=%q status=%d", req.URL.String(), resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return modelListDetection{}, err
	}
	setDebugf("models probe body url=%q bytes=%d preview=%q", req.URL.String(), len(body), debugBodyPreview(body, 1200))
	if resp.StatusCode != http.StatusOK {
		return modelListDetection{}, modelProbeHTTPError{
			statusCode: resp.StatusCode,
			status:     resp.Status,
			preview:    debugBodyPreview(body, 300),
		}
	}
	result, err := parseModelListForDetection(body)
	if err != nil {
		setDebugf("models probe parse failed url=%q err=%v", req.URL.String(), err)
		return modelListDetection{}, err
	}
	setDebugf("models probe parsed url=%q model_count=%d shape=%q", req.URL.String(), countCSV(result.models), result.shape)
	return result, nil
}

func debugBodyPreview(body []byte, max int) string {
	text := strings.TrimSpace(string(body))
	text = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(text)
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	if max > 0 && len(text) > max {
		return text[:max] + "...(truncated)"
	}
	return text
}

func parseModelListForDetection(body []byte) (modelListDetection, error) {
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return modelListDetection{}, fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(response.Data))
	for _, item := range response.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}

	return modelListDetection{
		models: strings.Join(models, ","),
		shape:  inferModelListShape(body),
	}, nil
}

type anthropicModelListForDetection struct {
	Data         []anthropicModelForDetection `json:"data"`
	FirstID      string                       `json:"first_id"`
	FirstId      string                       `json:"firstId"`
	LastID       string                       `json:"last_id"`
	LastId       string                       `json:"lastId"`
	HasMore      *bool                        `json:"has_more"`
	HasMoreCamel *bool                        `json:"hasMore"`
}

type anthropicModelForDetection struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type openAIModelListForDetection struct {
	Object string                    `json:"object"`
	Data   []openAIModelForDetection `json:"data"`
}

type openAIModelForDetection struct {
	ID          string          `json:"id"`
	Object      string          `json:"object"`
	Created     *int            `json:"created"`
	OwnedBy     string          `json:"owned_by"`
	Features    json.RawMessage `json:"features"`
	Modalities  json.RawMessage `json:"modalities"`
	TaskType    json.RawMessage `json:"task_type"`
	TokenLimits json.RawMessage `json:"token_limits"`
}

func inferModelListShape(body []byte) modelListShape {
	anthropic := responseMatchesAnthropicModelList(body)
	openAI := responseMatchesOpenAIModelList(body)

	if openAI && !anthropic {
		return modelListShapeOpenAI
	}
	if anthropic && !openAI {
		return modelListShapeAnthropic
	}
	return modelListShapeUnknown
}

func responseMatchesAnthropicModelList(body []byte) bool {
	var response anthropicModelListForDetection
	if err := json.Unmarshal(body, &response); err != nil || len(response.Data) == 0 {
		return false
	}

	if response.FirstID != "" || response.FirstId != "" || response.LastID != "" || response.LastId != "" || response.HasMore != nil || response.HasMoreCamel != nil {
		return true
	}

	for _, item := range response.Data {
		if item.ID == "" {
			continue
		}
		if item.Type != "" || item.CreatedAt != "" {
			return true
		}
	}
	return false
}

func responseMatchesOpenAIModelList(body []byte) bool {
	var response openAIModelListForDetection
	if err := json.Unmarshal(body, &response); err != nil || len(response.Data) == 0 {
		return false
	}

	if response.Object != "" {
		return true
	}

	for _, item := range response.Data {
		if item.ID == "" {
			continue
		}
		if item.Object != "" || item.Created != nil || item.OwnedBy != "" || len(item.Features) > 0 || len(item.Modalities) > 0 || len(item.TaskType) > 0 || len(item.TokenLimits) > 0 {
			return true
		}
	}
	return false
}
