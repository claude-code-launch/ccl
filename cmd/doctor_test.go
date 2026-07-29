package cmd

import (
	"testing"

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

func TestEffectiveCompactThresholdPrefersAbsoluteWindow(t *testing.T) {
	if got, label := effectiveCompactThreshold(1_000_000, 900_000); got != 900_000 || label == "" {
		t.Fatalf("threshold = %d (%q), want the auto-compact window", got, label)
	}
	if got, _ := effectiveCompactThreshold(500_000, 0); got != 500_000 {
		t.Fatalf("threshold = %d, want the assumed context size", got)
	}
	if got, label := effectiveCompactThreshold(0, 0); got != 0 || label != "" {
		t.Fatalf("threshold = %d (%q), want no override", got, label)
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
