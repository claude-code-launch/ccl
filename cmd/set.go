package cmd

import (
	"fmt"
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
		return RunConfSet(args)
	},
}

func RunConfSet(args []string) error {
	cfg, err := config.Load()
	if err != nil {
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
	program := tea.NewProgram(m)
	finalModel, err := program.Run()
	if err != nil {
		return fmt.Errorf("failed running advanced config panel: %w", err)
	}

	updatedModel := finalModel.(*AdvancedConfigModel)
	p = *updatedModel.p

	// 协议探测/模型获取失败 → 直接退出，不保存
	if updatedModel.detectionError != nil {
		fmt.Fprintf(os.Stderr, "❌ %s\n   %v\n",
			locale.T("协议探测与模型获取均失败，已退出配置", "protocol detection and model fetching both failed; aborted"),
			updatedModel.detectionError)
		return updatedModel.detectionError
	}

	// 未完成探测就退出（如在凭据页按 Esc）→ 不保存半成品配置
	if p.Endpoint == "" || p.APIKey == "" || p.Type == "" || p.Model == "" {
		fmt.Fprintln(os.Stderr, locale.T("ℹ️ 配置未完成，已退出，未保存。", "ℹ️ Configuration incomplete; aborted without saving."))
		return nil
	}

	// 1M 后置处理
	hasAny1M := false
	apply1M := func(slotName string, ptr *string) {
		if ptr == nil || *ptr == "" {
			return
		}
		if updatedModel.oneMSlots[slotName] {
			*ptr = *ptr + "[1m]"
			hasAny1M = true
		}
	}
	apply1M("opus", &p.OpusModel)
	apply1M("sonnet", &p.SonnetModel)
	apply1M("haiku", &p.HaikuModel)
	apply1M("custom", &p.CustomModelID)
	if p.CustomModelID != "" {
		p.LockModel = ""
	}

	if hasAny1M {
		if p.Env == nil {
			p.Env = make(map[string]string)
		}
		p.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = "1000000"
	}

	cfg.Providers[p.Name] = p
	if updatedModel.IsActiveChosen {
		cfg.ActiveProvider = p.Name
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Println("")
	if isUpdate {
		fmt.Printf("✅ %s %q\n", locale.T("已更新 Provider", "Successfully updated provider"), p.Name)
	} else {
		fmt.Printf("✅ %s %q\n", locale.T("已添加 Provider", "Successfully added provider"), p.Name)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(setCmd)
}

func SetCMD() *cobra.Command {
	return setCmd
}

func stringInSlice(s string, slice []string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// detectProtocolAndModels probes, in priority order: Anthropic Messages, then
// OpenAI-compatible (which is further split into the "openai(agent)" Responses
// protocol and the "openai(chat)" Chat Completions protocol — see
// detectOpenAIVariant). Returns (protocol, comma-separated-models, error).
// error is non-nil when every API call fails (endpoint unreachable or auth
// rejected); the returned protocol is then just a URL-based guess and models
// is empty.
func detectProtocolAndModels(endpoint, apiKey string) (string, string, error) {
	endpoint = strings.TrimSuffix(endpoint, "/")
	if models, err := protocol.GetAnthropicModels(endpoint, apiKey); err == nil {
		return "anthropic", models, nil
	}
	if models, err := protocol.GetOpenAIModels(endpoint, apiKey); err == nil {
		return detectOpenAIVariant(endpoint, apiKey, models), models, nil
	}
	// 两种协议都连不上：按 URL 猜一个协议，模型为空，返回错误
	guess := "openai"
	if strings.Contains(endpoint, "anthropic.com") {
		guess = "anthropic"
	}
	return guess, "", fmt.Errorf("%s", locale.T(
		"无法连接到端点或鉴权失败，请检查 URL 和 API Key",
		"failed to connect to endpoint or authenticate; please check URL and API Key",
	))
}

// openAIResponsesProbeTimeout bounds each concurrent /v1/responses probe request
// issued by detectOpenAIVariant.
const openAIResponsesProbeTimeout = 6 * time.Second

// maxOpenAIResponsesProbeCandidates caps how many discovered models are probed
// against the Responses endpoint, keeping detection latency bounded on gateways
// that expose long model catalogs.
const maxOpenAIResponsesProbeCandidates = 5

// detectOpenAIVariant determines which OpenAI-compatible protocol variant an
// endpoint actually implements:
//   - "openai_responses" — the newer, agent-oriented Responses API, displayed
//     to users as "openai(agent)".
//   - "openai" — the legacy Chat Completions API, displayed as "openai(chat)".
//
// Listing models (/v1/models) alone cannot tell these apart since both
// protocols commonly share the same model catalog, so this issues real,
// minimal generation requests to /v1/responses for a handful of discovered
// models concurrently. If any of them succeeds, the gateway is treated as
// supporting the agent protocol (preferred, since it is the more capable,
// forward-looking one); otherwise it falls back to plain Chat Completions.
func detectOpenAIVariant(endpoint, apiKey, modelsCSV string) string {
	var candidates []string
	for _, model := range strings.Split(modelsCSV, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		candidates = append(candidates, model)
		if len(candidates) == maxOpenAIResponsesProbeCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return "openai"
	}

	results := make(chan bool, len(candidates))
	for _, model := range candidates {
		go func(m string) {
			results <- protocol.ProbeOpenAIResponsesSupport(endpoint, apiKey, m, openAIResponsesProbeTimeout)
		}(model)
	}
	for range candidates {
		if <-results {
			return "openai_responses"
		}
	}
	return "openai"
}