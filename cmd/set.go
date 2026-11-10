package cmd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	// 🔥 引入 ccl 统一的国际化组件
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/modelrouting"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"

	// 统一使用指定的私有域 v2 包
	tui "github.com/grindlemire/go-tui"
)

// maxModelListProbeBytes caps how much of a model-list response is buffered while
// detecting whether an endpoint speaks OpenAI or Anthropic.
const maxModelListProbeBytes = 8 << 20

var setCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Add or update an LLM provider configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if refresh, _ := cmd.Flags().GetBool("refresh-models"); refresh {
			if len(args) != 0 {
				return fmt.Errorf("--refresh-models takes no provider name")
			}
			return refreshModelsDevMetadata(cmd.Context(), cmd.OutOrStdout())
		}
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
	var p provider.Provider
	if len(args) > 0 {
		targetName = strings.TrimSpace(args[0])
	} else {
		// 没有传 provider name：显示已有 provider 列表与新建入口。选中后进
		// 单页配置——URL / Key 直接在配置页顶部的 Connection 区填写，无需单
		// 独的凭据步骤。models.dev provider 在配置页的 Provider 行内选取。
		var names []string
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)

		newLabel := locale.T("+ 新建 Provider", "+ Create new provider")
		var providerItems []string
		providerItems = append(providerItems, newLabel)
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

		switch chosen {
		case newLabel:
			targetName = ""
		default:
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

	isUpdate := false
	if targetName != "" {
		if existing, exists := cfg.Providers[targetName]; exists {
			p = existing
			isUpdate = true
		} else {
			p.Name = targetName
		}
	}
	nameGenerated := false
	if targetName == "" {
		targetName = randomProviderName(cfg.Providers)
		p.Name = targetName
		nameGenerated = true
	}

	if p.OAuthProvider != "" {
		if err := provider.ValidateRuntime(p); err != nil {
			return err
		}
	}
	// 🚀 运行基于特定域 v2 架构的超级大面板
	m := NewAdvancedConfigModel(&p)
	m.NameGenerated = nameGenerated
	if p.OAuthProvider != "" {
		// The runtime refreshes a credential and fetches its catalog on the way
		// up, which can take tens of seconds. Starting it here would leave the
		// terminal blank for that whole time, so it runs in the background and
		// the panel opens with the persisted catalog instead.
		m.beginOAuthRuntime(p)
	}
	app, err := tui.NewApp(tui.WithRootComponent(m))
	if err != nil {
		setDebugf("advanced config panel failed err=%v", err)
		return fmt.Errorf("failed running advanced config panel: %w", err)
	}
	defer app.Close()
	defer m.stopOAuthRuntime()
	if err := app.Run(); err != nil {
		setDebugf("advanced config panel failed err=%v", err)
		return fmt.Errorf("failed running advanced config panel: %w", err)
	}

	updatedModel := m
	// models.dev providers never run the probe (their endpoint/protocol come from
	// metadata), so the API key the user typed lives only in the text input. Sync
	// it into the draft before validating/persisting. Harmless for normal
	// providers (the detection step already copied these values in).
	updatedModel.syncConnectionInputs()
	p = *updatedModel.p
	setDebugf(
		"advanced config finished name=%q endpoint_empty=%t api_key_len=%d type=%q model_count=%d effort=%q slots=%s one_m=%s detecting=%t detection_error=%v model_pool_count=%d connection_dirty=%t auto_configured=%t",
		p.Name,
		p.Endpoint == "",
		len(p.APIKey),
		p.Type,
		countCSV(p.Model),
		p.EffortLevel,
		slotDebugSummary(p),
		reviewOneMSummary(updatedModel.live().oneMSlots),
		updatedModel.live().detecting,
		updatedModel.live().detectionError,
		len(updatedModel.live().modelPool),
		updatedModel.live().connectionDirty,
		updatedModel.live().autoConfigured,
	)
	if !updatedModel.saveConfirmed {
		setDebugf("abort: save was not confirmed")
		fmt.Fprintln(os.Stderr, locale.T("ℹ️ 已取消配置，未保存。", "ℹ️ Configuration canceled; no changes were saved."))
		return nil
	}

	// 连接被修改但未重新检测，或从未成功探测过 → 不允许保存半成品。
	if !updatedModel.canSave() {
		setDebugf("abort: cannot save endpoint_dirty=%t detected=%t", updatedModel.live().connectionDirty, updatedModel.live().modelPoolFromDiscovery)
		fmt.Fprintln(os.Stderr, locale.T(
			"ℹ️ 连接已修改或尚未成功检测，未保存。请重新运行 Auto Configure。",
			"ℹ️ Connection changed or not detected; nothing was saved. Re-run Auto Configure.",
		))
		return nil
	}

	// 协议探测/模型获取失败 → 直接退出，不保存
	if updatedModel.live().detectionError != nil {
		setDebugf("abort: detection error err=%v", updatedModel.live().detectionError)
		fmt.Fprintf(os.Stderr, "❌ %s\n   %v\n",
			locale.T("协议探测与模型获取均失败，已退出配置", "protocol detection and model fetching both failed; aborted"),
			updatedModel.live().detectionError)
		return updatedModel.live().detectionError
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

	applyCompactConfig(&p, updatedModel.live().oneMSlots, updatedModel.live().compactPreset)
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

	// The page can rename the draft (a generated placeholder takes the
	// models.dev catalog ID). A name that now belongs to a different provider
	// must not be overwritten — that would silently replace its key and setup.
	// The page may have been open for minutes: decide the name and write against
	// the latest file, under its lock.
	requestedName := p.Name
	collided := false
	err = config.Update(func(latest *provider.Config) error {
		p.Name, collided = providerSaveName(latest.Providers, targetName, requestedName)
		latest.Providers[p.Name] = p
		if updatedModel.IsActiveChosen {
			latest.ActiveProvider = p.Name
		}
		return nil
	})
	if collided {
		fmt.Fprintf(os.Stderr, locale.T(
			"ℹ️ 已有名为 %q 的 Provider，本次保存为 %q（可用 ccl provider mv 改名）\n",
			"ℹ️ A provider named %q already exists; saved this one as %q (rename with ccl provider mv)\n",
		), requestedName, p.Name)
	}
	if err != nil {
		setDebugf("config save failed err=%v", err)
		return fmt.Errorf("failed to save config: %w", err)
	}
	setDebugf("config saved provider=%q active=%t", p.Name, updatedModel.IsActiveChosen)
	_ = refreshModelsDevMetadata(context.Background(), os.Stdout)

	fmt.Println("")
	if isUpdate {
		fmt.Printf("✅ %s %q\n", locale.T("已更新 Provider", "Successfully updated provider"), p.Name)
	} else {
		fmt.Printf("✅ %s %q\n", locale.T("已添加 Provider", "Successfully added provider"), p.Name)
	}
	printCodexModelUpdateNotice(os.Stdout, p.Name)
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
	setCmd.Flags().Bool("refresh-models", false, "Refresh models.dev metadata and its offline cache without editing providers")
	rootCmd.AddCommand(setCmd)
}

func SetCMD() *cobra.Command {
	return setCmd
}

func setDebugf(format string, args ...any) {
	oauthproxy.LogDebugf(format, args...)
}

// randomProviderName generates a short, unique, human-friendly name for a new
// provider created without an explicit name. It reads from crypto/rand and
// retries against the existing names so a fresh provider never collides with one
// already configured. The alphabet omits 0/O/1/l/I so the name stays legible.
func randomProviderName(existing map[string]provider.Provider) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz23456789"
	for range 64 {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			break
		}
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		name := "provider-" + string(b[:])
		if _, exists := existing[name]; !exists {
			return name
		}
	}
	// 64 collisions is astronomically unlikely; fall back to a timestamp so the
	// name remains unique even if the entropy source somehow keeps repeating.
	return fmt.Sprintf("provider-%d", time.Now().UnixNano())
}

// providerSaveName decides the key a provider edited as openedAs is saved
// under. Keeping its own name is always fine; a name the page assigned that now
// belongs to another provider gets a free suffix instead (collided=true).
func providerSaveName(existing map[string]provider.Provider, openedAs, name string) (string, bool) {
	if name == openedAs && !provider.IsDefaultProvider(name) {
		return name, false
	}
	if _, taken := existing[name]; !taken && !provider.IsDefaultProvider(name) {
		return name, false
	}
	return uniqueProviderName(existing, name), true
}

// uniqueProviderName returns base, or base-2, base-3, ... — the first name not
// already configured.
func uniqueProviderName(existing map[string]provider.Provider, base string) string {
	if _, taken := existing[base]; !taken && !provider.IsDefaultProvider(base) {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, taken := existing[candidate]; !taken {
			return candidate
		}
	}
}

func countCSV(csv string) int {
	return len(modelrouting.SplitCSV(csv))
}

func slotDebugSummary(p provider.Provider) string {
	return fmt.Sprintf(
		"opus_set=%t sonnet_set=%t haiku_set=%t fable_set=%t custom_set=%t subagent_set=%t",
		strings.TrimSpace(p.OpusModel) != "",
		strings.TrimSpace(p.SonnetModel) != "",
		strings.TrimSpace(p.HaikuModel) != "",
		strings.TrimSpace(p.FableModel) != "",
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

type protocolDetectionResult struct {
	protocol   string
	models     string
	modelInfos []protocol.ModelInfo
	baseURL    string
	err        error
}

// A models list verifies credentials. Chat and Responses share that list,
// so preserve a configured Responses choice without a generation probe.
func detectProtocolAndModelsPreferred(endpoint, apiKey string, preference ...string) protocolDetectionResult {
	base := normalizeModelBaseURL(endpoint)
	if base == "" || strings.TrimSpace(apiKey) == "" {
		return protocolDetectionResult{err: fmt.Errorf("endpoint and API key are required")}
	}
	req, err := http.NewRequest(http.MethodGet, protocol.NormalizeOpenAIModelsURL(base), nil)
	if err != nil {
		return protocolDetectionResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	result, err := fetchModelListForDetection(req, 8*time.Second)
	if err != nil {
		return protocolDetectionResult{err: fmt.Errorf("OpenAI model-list probe failed: %w", err)}
	}
	if result.shape == modelListShapeAnthropic {
		return protocolDetectionResult{err: fmt.Errorf("this endpoint serves the retired Anthropic Messages protocol; enter a Responses or Chat Completions endpoint")}
	}
	if result.models == "" {
		return protocolDetectionResult{err: fmt.Errorf("endpoint returned no models")}
	}
	typ := "openai"
	if len(preference) > 0 && provider.IsOpenAIResponsesType(preference[0]) {
		typ = "openai_responses"
	}
	return protocolDetectionResult{protocol: typ, models: result.models, modelInfos: result.modelInfos, baseURL: base}
}

func detectProtocolAndModelsDetailed(endpoint, apiKey string) protocolDetectionResult {
	return detectProtocolAndModelsPreferred(endpoint, apiKey)
}

type modelListShape string

const (
	modelListShapeUnknown   modelListShape = ""
	modelListShapeOpenAI    modelListShape = "openai"
	modelListShapeAnthropic modelListShape = "anthropic"
)

type modelListDetection struct {
	models     string
	modelInfos []protocol.ModelInfo
	shape      modelListShape
}

func normalizeModelBaseURL(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case strings.HasSuffix(endpoint, "/models"):
		return strings.TrimSuffix(endpoint, "/models")
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
	// A misconfigured endpoint can point at anything, so never buffer an unbounded
	// response just to sniff a model list out of it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListProbeBytes))
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
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			DisplayName        string   `json:"display_name"`
			ContextWindow      int      `json:"context_window"`
			MaxInputTokens     int      `json:"max_input_tokens"`
			MaxOutputTokens    int      `json:"max_output_tokens"`
			RateMultiplier     *float64 `json:"rate_multiplier"`
			RateUnit           string   `json:"rate_unit"`
			IsNew              bool     `json:"is_new"`
			PromotionAvailable bool     `json:"promotion_available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return modelListDetection{}, fmt.Errorf("%s: %w", locale.T("解析响应失败", "decode models response"), err)
	}

	models := make([]string, 0, len(response.Data))
	infos := make([]protocol.ModelInfo, 0, len(response.Data))
	for _, item := range response.Data {
		if item.ID != "" {
			models = append(models, item.ID)
			displayName := strings.TrimSpace(item.DisplayName)
			if displayName == "" {
				displayName = strings.TrimSpace(item.Name)
			}
			contextWindow := item.MaxInputTokens
			if contextWindow == 0 {
				contextWindow = item.ContextWindow
			}
			infos = append(infos, protocol.ModelInfo{
				ID: item.ID, DisplayName: displayName, ContextWindow: contextWindow,
				MaxOutputTokens: item.MaxOutputTokens, RateMultiplier: item.RateMultiplier,
				RateUnit: item.RateUnit, IsNew: item.IsNew, PromotionAvailable: item.PromotionAvailable,
			})
		}
	}

	return modelListDetection{
		models:     strings.Join(models, ","),
		modelInfos: infos,
		shape:      inferModelListShape(body),
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
