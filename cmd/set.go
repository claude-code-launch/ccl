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

	applyOneMConfig(&p, updatedModel.oneMSlots)

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
// OpenAI-compatible Chat Completions. Responses remains supported for manually
// authored provider configs, but automatic setup intentionally does not select
// it: several Codex-oriented /responses gateways accept simple probes while
// rejecting real Claude Code agent traffic unless it comes from the official
// Codex client.
// Returns (protocol, comma-separated-models, error).
// error is non-nil when every supported protocol probe fails; protocol and
// models are then empty.
func detectProtocolAndModels(endpoint, apiKey string) (string, string, error) {
	result := detectProtocolAndModelsDetailed(endpoint, apiKey)
	return result.protocol, result.models, result.err
}

type protocolDetectionResult struct {
	protocol      string
	models        string
	anthropicAuth string
	err           error
}

const (
	anthropicAuthXAPIKey = "x-api-key"
	anthropicAuthBearer  = "bearer"
)

func detectProtocolAndModelsDetailed(endpoint, apiKey string) protocolDetectionResult {
	endpoint = strings.TrimSuffix(endpoint, "/")
	if models, err := protocol.GetAnthropicModels(endpoint, apiKey); err == nil {
		if probeAnthropicMessagesWithAuth(endpoint, apiKey, models, anthropicAuthXAPIKey) {
			return protocolDetectionResult{protocol: "anthropic", models: models, anthropicAuth: anthropicAuthXAPIKey}
		}
	}
	if models, err := protocol.GetAnthropicModelsWithAuth(endpoint, apiKey, anthropicAuthBearer); err == nil {
		if probeAnthropicMessagesWithAuth(endpoint, apiKey, models, anthropicAuthBearer) {
			return protocolDetectionResult{protocol: "anthropic", models: models, anthropicAuth: anthropicAuthBearer}
		}
	}
	if models, err := protocol.GetOpenAIModels(endpoint, apiKey); err == nil {
		if authStyle, ok := detectAnthropicMessagesVariant(endpoint, apiKey, models); ok {
			return protocolDetectionResult{protocol: "anthropic", models: models, anthropicAuth: authStyle}
		}
		if probeOpenAIChat(endpoint, apiKey, models) {
			return protocolDetectionResult{protocol: "openai", models: models}
		}
	}
	return protocolDetectionResult{err: fmt.Errorf("%s", locale.T(
		"暂不支持这个协议：Anthropic Messages 与 OpenAI Chat Completions 探测均失败",
		"unsupported protocol: both Anthropic Messages and OpenAI Chat Completions probes failed",
	))}
}

// anthropicMessagesProbeTimeout bounds each concurrent /v1/messages probe used
// when a gateway exposes OpenAI /models but only implements Anthropic /messages
// (not Anthropic /models). Several Anthropic-compatible routers have that shape.
const anthropicMessagesProbeTimeout = 6 * time.Second

const maxAnthropicMessagesProbeCandidates = 5

func detectAnthropicMessagesVariant(endpoint, apiKey, modelsCSV string) (string, bool) {
	if probeAnthropicMessagesWithAuth(endpoint, apiKey, modelsCSV, anthropicAuthXAPIKey) {
		return anthropicAuthXAPIKey, true
	}
	if probeAnthropicMessagesWithAuth(endpoint, apiKey, modelsCSV, anthropicAuthBearer) {
		return anthropicAuthBearer, true
	}
	return "", false
}

func probeAnthropicMessagesWithAuth(endpoint, apiKey, modelsCSV, authStyle string) bool {
	var candidates []string
	for _, model := range strings.Split(modelsCSV, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		candidates = append(candidates, model)
		if len(candidates) == maxAnthropicMessagesProbeCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return false
	}

	results := make(chan bool, len(candidates))
	for _, model := range candidates {
		go func(m string) {
			results <- testSingleAnthropicModelWithAuth(m, endpoint, apiKey, authStyle, anthropicMessagesProbeTimeout)
		}(model)
	}
	for range candidates {
		if <-results {
			return true
		}
	}
	return false
}

// openAIChatProbeTimeout bounds each concurrent /v1/chat/completions probe used
// after /v1/models succeeds. Some gateways expose a model list but do not
// implement Chat Completions, so listing models alone is not enough.
const openAIChatProbeTimeout = 6 * time.Second

const maxOpenAIChatProbeCandidates = 5

func probeOpenAIChat(endpoint, apiKey, modelsCSV string) bool {
	var candidates []string
	for _, model := range strings.Split(modelsCSV, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		candidates = append(candidates, model)
		if len(candidates) == maxOpenAIChatProbeCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return false
	}

	results := make(chan bool, len(candidates))
	for _, model := range candidates {
		go func(m string) {
			results <- testSingleOpenAIModel(m, endpoint, apiKey, openAIChatProbeTimeout)
		}(model)
	}
	for range candidates {
		if <-results {
			return true
		}
	}
	return false
}
