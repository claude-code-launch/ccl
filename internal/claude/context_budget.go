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

const (
	// claudeDefaultContextWindow is the window Claude Code assumes for a model it
	// does not recognize, which is every model behind a gateway.
	claudeDefaultContextWindow = 200_000
	// oneMClassContextWindow is the smallest window treated as 1M-class. Claude
	// Code effectively supports two sizes, its 200K default and a 1M-class window
	// selected per slot with the [1m] marker; declaring a size in between for a
	// gateway model is honored inconsistently (the status line, the compact buffer
	// and the trigger point disagree), so ccl does not ask for one.
	oneMClassContextWindow = 900_000
)

// declaredContextWindow maps an advertised window onto a size Claude Code handles
// predictably: itself when it is 1M-class or at most the default, and the 200K
// default for anything in between.
//
// A 500K model is the awkward case this exists for: claiming 500K risks the
// mismatched-buffer behaviour, while 200K is always sized correctly and simply
// leaves capacity unused.
func DeclaredContextWindow(advertised int) int { return declaredContextWindow(advertised) }

// MappedContextClassesDiffer reports whether the provider's mapped models fall
// into more than one context class, which is when no session-wide context value
// can be correct for all of them.
func MappedContextClassesDiffer(p provider.Provider, windows map[string]int) bool {
	classes := make(map[int]struct{}, 2)
	for _, slot := range provider.SlotModels(p) {
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			classes[contextClass(window)] = struct{}{}
		}
	}
	return len(classes) > 1
}

func declaredContextWindow(advertised int) int {
	switch {
	case advertised <= 0:
		return 0
	case advertised >= oneMClassContextWindow, advertised <= claudeDefaultContextWindow:
		return advertised
	default:
		return claudeDefaultContextWindow
	}
}

// ManagedContextBudget is the context sizing a subscription backend reports for
// the models mapped in a session.
//
// Subscription backends own this number and change it without notice (OpenAI cut
// the Codex window for GPT-5.6 from 372K to 272K in a point release), so for
// OAuth providers ccl follows the advertised value instead of a preset the user
// picked once.
type ManagedContextBudget struct {
	// Advertised is the smallest context window advertised across the mapped slots.
	Advertised int
	// Window is the size handed to Claude Code, which is Advertised reduced to a
	// size Claude Code sizes sessions for predictably. Zero means ccl declares
	// nothing and Claude Code applies its own per-model sizing.
	Window int
	// CompactWindow is the auto-compact threshold derived from Window.
	CompactWindow int
	// Model is the mapped model that Advertised came from.
	Model string
	// Source names the catalog the window was read from.
	Source string
	// Mixed reports that the mapped models fall into more than one context class.
	// The context env is session-global, so no single value can be right for all
	// of them and ccl declares nothing instead.
	Mixed bool
}

// contextClass buckets an advertised window into the two sizes Claude Code sizes
// sessions for.
func contextClass(window int) int {
	if window >= oneMClassContextWindow {
		return oneMClassContextWindow
	}
	return claudeDefaultContextWindow
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
	if provider.ContextBudgetIsManual(p) {
		// The advertised window can be a client-side catalog cap rather than the
		// server's real limit, so an explicit opt-out must be able to aim higher.
		oauthproxy.Debugf("context budget manual override provider=%q max_context_tokens=%q auto_compact_window=%q",
			p.Name, p.Env[provider.EnvMaxContextTokens], p.Env[provider.EnvAutoCompactWindow])
		return ManagedContextBudget{}, false
	}
	windows, source := AdvertisedContextWindows(p.Endpoint, p.APIKey)
	if len(windows) == 0 {
		return ManagedContextBudget{}, false
	}
	budget := ManagedContextBudget{Source: source}
	classes := make(map[int]struct{}, 2)
	for _, slot := range provider.SlotModels(p) {
		window, ok := windows[strings.ToLower(slot.Model)]
		if !ok || window <= 0 {
			continue
		}
		classes[contextClass(window)] = struct{}{}
		if budget.Advertised == 0 || window < budget.Advertised {
			budget.Advertised, budget.Model = window, slot.Model
		}
	}
	if budget.Advertised == 0 {
		return ManagedContextBudget{}, false
	}
	// The context env applies to the whole session while the [1m] marker is
	// per slot. When the mapped models span both classes, any global number is
	// wrong for one of them: too large and the small model's requests are
	// rejected, too small and the large model is throttled. Claude Code's own
	// per-model sizing is at least self-consistent, so step aside.
	if len(classes) > 1 {
		budget.Mixed = true
		return budget, true
	}
	budget.Window = declaredContextWindow(budget.Advertised)
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
	if env == nil {
		return
	}
	if budget.Mixed {
		// A stored global preset would apply to every slot, including the models it
		// does not fit, so drop it and let Claude Code size each model itself.
		delete(env, provider.EnvMaxContextTokens)
		delete(env, provider.EnvAutoCompactWindow)
		return
	}
	if budget.Window <= 0 {
		return
	}
	env[provider.EnvMaxContextTokens] = strconv.Itoa(budget.Window)
	if budget.CompactWindow > 0 {
		env[provider.EnvAutoCompactWindow] = strconv.Itoa(budget.CompactWindow)
	}
}
