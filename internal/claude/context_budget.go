package claude

import (
	"strings"
	"sync"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// Context sizing policy
//
// Claude Code has two sizings: its 200K default, and a 1M-class window selected
// per slot by the [1m] marker on a model id. It scales its own compaction buffer
// and trigger point to whichever one a slot uses.
//
// ccl offers exactly two provider-wide modes:
//
//   - Default declares nothing and preserves Claude Code's native 200K/1M
//     behavior selected per slot by the [1m] marker.
//   - Balanced declares a 500K context/window and an 80% compact threshold,
//     which triggers compaction at approximately 400K.
//
// Other historical or hand-written combinations are removed at launch so the
// effective behavior always matches one of those two choices. The per-slot [1m]
// marker remains independently configurable; Balanced intentionally applies a
// single 500K cap to every slot.
const (
	// claudeDefaultContextWindow is the window Claude Code assumes for a model it
	// does not recognize, which is every model behind a gateway.
	claudeDefaultContextWindow = 200_000
	// oneMClassContextWindow is the smallest window treated as 1M-class.
	oneMClassContextWindow = 900_000
)

// contextClass buckets an advertised window into the two sizings Claude Code has.
func contextClass(window int) int {
	if window >= oneMClassContextWindow {
		return oneMClassContextWindow
	}
	return claudeDefaultContextWindow
}

// ContextClassLabel names the sizing Claude Code will use for an advertised
// window, for diagnostics.
func ContextClassLabel(window int) string {
	if window <= 0 {
		return "unknown"
	}
	if contextClass(window) == oneMClassContextWindow {
		return "1M-class"
	}
	return "200K default"
}

// MappedContextClassesDiffer reports whether the provider's mapped models fall
// into more than one context class, which is when no session-wide context value
// could be correct for all of them.
func MappedContextClassesDiffer(p provider.Provider, windows map[string]int) bool {
	classes := make(map[int]struct{}, 2)
	for _, slot := range provider.SlotModels(p) {
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			classes[contextClass(window)] = struct{}{}
		}
	}
	return len(classes) > 1
}

// AdvertisedContextWindows reads the context window of every model a runtime
// exposes, keyed by lowercased model id, plus the catalog it came from.
//
// Subscription runtimes only reveal windows through the Codex client catalog; the
// plain OpenAI list is trimmed to id/object/created/owned_by. Both shapes are
// tried so API-key gateways still work. The values are advisory: they may be a
// client-side catalog cap rather than what the server enforces.
func AdvertisedContextWindows(endpoint, apiKey string) (map[string]int, string) {
	sources := []struct {
		label string
		fetch func(string, string) ([]protocol.ModelInfo, error)
	}{
		{"Codex client catalog", protocol.GetCodexClientModelInfos},
		{"OpenAI /models", protocol.GetOpenAIModelInfos},
	}
	// Probe both shapes at once: sequentially this costs two request timeouts on
	// every gateway that serves only one of them, which is a visible stall in
	// `ccl doctor` and in the config TUI.
	type result struct {
		windows map[string]int
		label   string
	}
	results := make([]result, len(sources))
	var wait sync.WaitGroup
	for index, source := range sources {
		wait.Add(1)
		go func() {
			defer wait.Done()
			infos, err := source.fetch(endpoint, apiKey)
			if err != nil {
				oauthproxy.LogWarnf("context window catalog %q failed: %v", source.label, err)
				return
			}
			windows := make(map[string]int, len(infos))
			for _, info := range infos {
				if info.ContextWindow > 0 {
					windows[strings.ToLower(info.ID)] = info.ContextWindow
				}
			}
			if len(windows) > 0 {
				results[index] = result{windows: windows, label: source.label}
			}
		}()
	}
	wait.Wait()
	// Source order still decides the winner, so the Codex catalog keeps priority.
	for _, candidate := range results {
		if len(candidate.windows) > 0 {
			return candidate.windows, candidate.label
		}
	}
	return nil, ""
}

// applyContextPolicy keeps only the exact Balanced triplet. Any other context
// override is retired to Default and removed from the launched session.
func applyContextPolicy(env map[string]string) bool {
	if env == nil || provider.IsBalancedContextPreset(env) {
		return false
	}
	if !provider.HasManagedContextEnv(env) {
		return false
	}
	for _, key := range provider.ManagedContextEnvKeys() {
		delete(env, key)
	}
	return true
}
