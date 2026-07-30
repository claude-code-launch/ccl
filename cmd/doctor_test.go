package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestModelVerificationSummary(t *testing.T) {
	tests := []struct {
		name        string
		available   []string
		unavailable []string
		want        string
	}{
		{name: "mixed", available: []string{"a", "b"}, unavailable: []string{"c"}, want: "2 available · 1 unavailable"},
		{name: "all available", available: []string{"a"}, want: "1 available · 0 unavailable"},
		{name: "all unavailable", unavailable: []string{"a", "b"}, want: "0 available · 2 unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelVerificationSummary(tt.available, tt.unavailable); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSmallestMappedWindowCountsUnknownModels(t *testing.T) {
	p := provider.Provider{
		OpusModel:   "gpt-5.6-sol[1m]",
		SonnetModel: "gpt-5.6-terra",
		HaikuModel:  "not-in-catalog",
	}
	windows := map[string]int{
		"gpt-5.6-sol":   272_000,
		"gpt-5.6-terra": 128_000,
	}
	smallest, model, unknown := smallestMappedWindow(p, windows)
	if smallest != 128_000 || model != "gpt-5.6-terra" {
		t.Fatalf("smallest = %d (%q)", smallest, model)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1", unknown)
	}
}

func TestFormatTokenCountIsReadableAndExact(t *testing.T) {
	cases := map[int]string{
		272_000:   "272K (272000)",
		1_000_000: "1M (1000000)",
		258_400:   "258400",
	}
	for tokens, want := range cases {
		if got := formatTokenCount(tokens); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tokens, got, want)
		}
	}
}

func TestPrintDoctorOneMConsistencyFlagsOversizedMarkers(t *testing.T) {
	// The check is output-only; assert the decision inputs it depends on, so a
	// [1m] slot whose backend window is small is recognizable.
	p := provider.Provider{
		OpusModel:   "gpt-5.6-sol[1m]",
		SonnetModel: "claude-sonnet-4-6[1m]",
		HaikuModel:  "gpt-5.6-luna",
	}
	slots := oneMSlotsFromProvider(p)
	if !slots["opus"] || !slots["sonnet"] || slots["haiku"] {
		t.Fatalf("one-M slots = %#v", slots)
	}
	windows := map[string]int{
		"gpt-5.6-sol":       272_000,
		"claude-sonnet-4-6": 1_000_000,
	}
	if protocol.ContextWindowSuggests1M(windows["gpt-5.6-sol"]) {
		t.Error("272K must not count as a 1M-class window")
	}
	if !protocol.ContextWindowSuggests1M(windows["claude-sonnet-4-6"]) {
		t.Error("1M must count as a 1M-class window")
	}
	// Must not panic or warn when no catalog is available.
	printDoctorOneMConsistency(p, nil)
}
