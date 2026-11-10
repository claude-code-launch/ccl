package cmd

import (
	"strings"
	"sync"

	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

const (
	// defaultContextWindow is the window Codex assumes for a model it
	// does not recognize, which is every model behind a gateway.
	defaultContextWindow = 200_000
	// oneMClassContextWindow is the smallest window treated as 1M-class.
	oneMClassContextWindow = 900_000
)

// contextClass buckets an advertised window into the two sizings Codex has.
func contextClass(window int) int {
	if window >= oneMClassContextWindow {
		return oneMClassContextWindow
	}
	return defaultContextWindow
}

// contextClassLabel names the sizing Codex will use for an advertised
// window, for diagnostics.
func contextClassLabel(window int) string {
	if window <= 0 {
		return "unknown"
	}
	if contextClass(window) == oneMClassContextWindow {
		return "1M-class"
	}
	return "200K default"
}

// mappedContextClassesDiffer reports whether the provider's mapped models fall
// into more than one context class, which is when no session-wide context value
// could be correct for all of them.
func mappedContextClassesDiffer(p provider.Provider, windows map[string]int) bool {
	classes := make(map[int]struct{}, 2)
	for _, slot := range provider.SlotModels(p) {
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			classes[contextClass(window)] = struct{}{}
		}
	}
	return len(classes) > 1
}

// advertisedContextWindows reads the context window of every model a runtime
// exposes, keyed by lowercased model id, plus the catalog it came from.
//
// Subscription runtimes only reveal windows through the Codex client catalog; the
// plain OpenAI list is trimmed to id/object/created/owned_by. Both shapes are
// tried so API-key gateways still work. The values are advisory: they may be a
// client-side catalog cap rather than what the server enforces.
func advertisedContextWindows(endpoint, apiKey string) (map[string]int, string) {
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
