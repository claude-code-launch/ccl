package cmd

import (
	"strings"

	"github.com/claude-code-launch/ccl/internal/provider"
)

const autoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

func applyOneMConfig(p *provider.Provider, oneMSlots map[string]bool) {
	hasAny1M := false
	apply := func(slotName string, ptr *string) {
		if ptr == nil {
			return
		}
		*ptr = stripOneMSuffix(*ptr)
		if *ptr == "" {
			return
		}
		if oneMSlots[slotName] {
			*ptr += "[1m]"
			hasAny1M = true
		}
	}

	apply("opus", &p.OpusModel)
	apply("sonnet", &p.SonnetModel)
	apply("haiku", &p.HaikuModel)
	apply("custom", &p.CustomModelID)

	if hasAny1M {
		if p.Env == nil {
			p.Env = make(map[string]string)
		}
		p.Env[autoCompactWindowEnv] = "1000000"
		return
	}

	if p.Env != nil {
		delete(p.Env, autoCompactWindowEnv)
		if len(p.Env) == 0 {
			p.Env = nil
		}
	}
}

func oneMSlotsFromProvider(p provider.Provider) map[string]bool {
	slots := make(map[string]bool)
	for _, slot := range []struct {
		name  string
		model string
	}{
		{"opus", p.OpusModel},
		{"sonnet", p.SonnetModel},
		{"haiku", p.HaikuModel},
		{"custom", p.CustomModelID},
	} {
		if hasOneMSuffix(slot.model) {
			slots[slot.name] = true
		}
	}
	return slots
}

func stripOneMSuffix(model string) string {
	model = strings.TrimSpace(model)
	for strings.HasSuffix(model, "[1m]") {
		model = strings.TrimSpace(strings.TrimSuffix(model, "[1m]"))
	}
	return model
}

func hasOneMSuffix(model string) bool {
	return strings.HasSuffix(strings.TrimSpace(model), "[1m]")
}
