package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/proxymanager"
)

func codexCatalogConnectionMessage(capabilities codex.CatalogCapabilities) string {
	if capabilities.SupportsRemoteFetch {
		return locale.T("CCL 路由与远程模型目录已接入。Codex 按客户端策略获取目录，可能需要重启才能刷新模型列表。", "CCL routes and remote catalog connected. Codex controls catalog fetching; a restart may be needed to refresh the model list.")
	}
	return locale.T("CCL 路由与本地模型目录已接入。Codex 可能需要重启才能刷新模型列表。", "CCL routes and local catalog connected. Codex may need a restart to refresh the model list.")
}

func codexModelUpdateMessage(state *proxymanager.State, name string) string {
	if !state.SupportsGatewayHotReload() || state.Provider != name && !slices.Contains(state.AdditionalProviders, name) {
		return ""
	}
	return locale.T("模型配置已保存。CCL 将自动更新路由与目录；Codex 可能需要重启才能刷新模型列表。", "Model configuration saved. CCL will update routes and catalog automatically; Codex may need a restart to refresh the model list.")
}

// Report only for a running, bound Codex provider. Saving ordinary Claude
// configuration does not add an unrelated Desktop message to the user flow.
func printCodexModelUpdateNotice(out io.Writer, name string) {
	manager, err := proxymanager.Default()
	if err != nil {
		return
	}
	state, err := manager.Read()
	if err != nil {
		return
	}
	message := codexModelUpdateMessage(state, name)
	if message == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if manager.Running(ctx, state) {
		fmt.Fprintln(out, message)
	}
}
