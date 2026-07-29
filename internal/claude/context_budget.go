package claude

import (
	"strconv"
	"strings"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// managedContextHeadroom is the fraction of the advertised window reserved for the
// reply and for the tokens compaction itself adds. Claude Code must start
// compacting before the hard limit, otherwise the upstream rejects the request
// with context_length_exceeded and no compaction ever runs.
const managedContextHeadroom = 20

// ManagedContextBudget is the context sizing a subscription backend reports for
// the models mapped in a session.
//
// Subscription backends own this number and change it without notice (OpenAI cut
// the Codex window for GPT-5.6 from 372K to 272K in a point release), so for
// OAuth providers ccl follows the advertised value instead of a preset the user
// picked once.
type ManagedContextBudget struct {
	// Window is the smallest context window advertised across the mapped slots.
	Window int
	// CompactWindow is the auto-compact threshold derived from Window.
	CompactWindow int
	// Model is the mapped model that Window came from.
	Model string
	// Source names the catalog the window was read from.
	Source string
}

// ManagedCompactWindow derives the auto-compact threshold for an advertised
// context window, rounded down to a whole thousand tokens.
func ManagedCompactWindow(window int) int {
	if window <= 0 {
		return 0
	}
	usable := window * (100 - managedContextHeadroom) / 100
	if usable >= 1000 {
		usable -= usable % 1000
	}
	if usable <= 0 {
		usable = window
	}
	return usable
}

// ResolveManagedContextBudget asks the runtime endpoint which context window the
// backend advertises for this provider's mapped models.
//
// Only subscription (OAuth) providers are managed: for API-key providers the user
// picks the endpoint and the preset, and there is no authoritative catalog to
// defer to. ok is false when the provider is not OAuth-backed or when no mapped
// model reports a window, in which case the configured preset stays in charge.
func ResolveManagedContextBudget(p provider.Provider) (ManagedContextBudget, bool) {
	if strings.TrimSpace(p.OAuthProvider) == "" {
		return ManagedContextBudget{}, false
	}
	windows, source := AdvertisedContextWindows(p.Endpoint, p.APIKey)
	if len(windows) == 0 {
		return ManagedContextBudget{}, false
	}
	budget := ManagedContextBudget{Source: source}
	for _, slot := range provider.SlotModels(p) {
		window, ok := windows[strings.ToLower(slot.Model)]
		if !ok || window <= 0 {
			continue
		}
		if budget.Window == 0 || window < budget.Window {
			budget.Window, budget.Model = window, slot.Model
		}
	}
	if budget.Window == 0 {
		return ManagedContextBudget{}, false
	}
	budget.CompactWindow = ManagedCompactWindow(budget.Window)
	return budget, true
}

// AdvertisedContextWindows reads the context window of every model a runtime
// exposes, keyed by lowercased model id, plus the catalog it came from.
//
// Subscription runtimes only reveal windows through the Codex client catalog; the
// plain OpenAI list is trimmed to id/object/created/owned_by. Both shapes are
// tried so API-key gateways still work.
func AdvertisedContextWindows(endpoint, apiKey string) (map[string]int, string) {
	sources := []struct {
		label string
		fetch func(string, string) ([]protocol.ModelInfo, error)
	}{
		{"Codex client catalog", protocol.GetCodexClientModelInfos},
		{"OpenAI /models", protocol.GetOpenAIModelInfos},
	}
	for _, source := range sources {
		infos, err := source.fetch(endpoint, apiKey)
		if err != nil {
			oauthproxy.Debugf("context window catalog %q failed: %v", source.label, err)
			continue
		}
		windows := make(map[string]int, len(infos))
		for _, info := range infos {
			if info.ContextWindow > 0 {
				windows[strings.ToLower(info.ID)] = info.ContextWindow
			}
		}
		if len(windows) > 0 {
			return windows, source.label
		}
	}
	return nil, ""
}

// applyManagedContextBudget replaces the ccl-managed context env with the values
// the backend advertises. It runs after the provider Env overrides so a stale
// stored preset cannot outrank the live catalog.
func applyManagedContextBudget(env map[string]string, budget ManagedContextBudget) {
	if env == nil || budget.Window <= 0 {
		return
	}
	env[provider.EnvMaxContextTokens] = strconv.Itoa(budget.Window)
	if budget.CompactWindow > 0 {
		env[provider.EnvAutoCompactWindow] = strconv.Itoa(budget.CompactWindow)
	}
}
