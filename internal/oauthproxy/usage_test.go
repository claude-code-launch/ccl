package oauthproxy

import (
	"strings"
	"testing"
)

func TestUsageTrackerAddAccumulatesPerModel(t *testing.T) {
	tracker := NewUsageTracker()
	tracker.Add("gpt-5.6-sol", 100, 50, 10, 5)
	tracker.Add("gpt-5.6-sol", 20, 10, 0, 0)
	tracker.Add("claude-opus-5", 5, 5, 0, 0)

	totals, ok := tracker.Snapshot()
	if !ok {
		t.Fatalf("expected Snapshot to report data present")
	}
	if len(totals) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(totals), totals)
	}
	// Snapshot is ordered by first use, not by size.
	if totals[0].Model != "gpt-5.6-sol" {
		t.Fatalf("expected first model to be gpt-5.6-sol (insertion order), got %q", totals[0].Model)
	}
	got := totals[0]
	if got.InputTokens != 120 || got.OutputTokens != 60 || got.CacheReadTokens != 10 || got.CacheWriteTokens != 5 || got.Requests != 2 {
		t.Fatalf("unexpected totals for gpt-5.6-sol: %+v", got)
	}
	if totals[1].Model != "claude-opus-5" || totals[1].Requests != 1 {
		t.Fatalf("unexpected totals for second model: %+v", totals[1])
	}
}

func TestUsageTrackerAddEmptyModelFallsBackToUnknown(t *testing.T) {
	tracker := NewUsageTracker()
	tracker.Add("  ", 1, 1, 0, 0)
	totals, ok := tracker.Snapshot()
	if !ok || len(totals) != 1 || totals[0].Model != "unknown" {
		t.Fatalf("expected a single \"unknown\" entry, got %+v (ok=%t)", totals, ok)
	}
}

func TestUsageTrackerNilIsSafe(t *testing.T) {
	var tracker *UsageTracker
	tracker.Add("model", 1, 1, 0, 0) // must not panic
	if totals, ok := tracker.Snapshot(); ok || totals != nil {
		t.Fatalf("expected nil tracker Snapshot to report nothing, got %+v ok=%t", totals, ok)
	}
}

func TestUsageTrackerSnapshotEmpty(t *testing.T) {
	tracker := NewUsageTracker()
	if totals, ok := tracker.Snapshot(); ok || totals != nil {
		t.Fatalf("expected empty tracker to report nothing, got %+v ok=%t", totals, ok)
	}
}

func TestFormatUsageSummaryEmpty(t *testing.T) {
	if got := FormatUsageSummary(nil); got != "" {
		t.Fatalf("expected empty summary for nil input, got %q", got)
	}
}

func TestFormatUsageSummarySingleModelHasNoTotalLine(t *testing.T) {
	totals := []UsageModelTotals{
		{Model: "claude-opus-5", UsageTotals: UsageTotals{InputTokens: 1000, OutputTokens: 200, Requests: 3}},
	}
	summary := FormatUsageSummary(totals)
	lines := strings.Split(summary, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line for a single model, got %d: %q", len(lines), summary)
	}
	if !strings.HasPrefix(lines[0], "[ccl usage] claude-opus-5: ") {
		t.Fatalf("unexpected line prefix: %q", lines[0])
	}
	if !strings.Contains(lines[0], "1K in") || !strings.Contains(lines[0], "200 out") || !strings.Contains(lines[0], "(3 req)") {
		t.Fatalf("unexpected line content: %q", lines[0])
	}
}

func TestFormatUsageSummarySortsBySizeAndAddsTotal(t *testing.T) {
	totals := []UsageModelTotals{
		{Model: "small-model", UsageTotals: UsageTotals{InputTokens: 10, OutputTokens: 5, Requests: 1}},
		{Model: "big-model", UsageTotals: UsageTotals{InputTokens: 2_000_000, OutputTokens: 500_000, Requests: 2}},
	}
	summary := FormatUsageSummary(totals)
	lines := strings.Split(summary, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 2 model lines + 1 total line, got %d: %q", len(lines), summary)
	}
	if !strings.Contains(lines[0], "big-model") {
		t.Fatalf("expected the larger model first, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "2M in") || !strings.Contains(lines[0], "500K out") {
		t.Fatalf("unexpected compact formatting: %q", lines[0])
	}
	if !strings.Contains(lines[1], "small-model") {
		t.Fatalf("expected the smaller model second, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "[ccl usage] total: ") {
		t.Fatalf("expected a trailing total line, got %q", lines[2])
	}
	if !strings.Contains(lines[2], "(3 req)") {
		t.Fatalf("expected total line to sum requests, got %q", lines[2])
	}
}

func TestFormatUsageSummaryIncludesCacheOnlyWhenPresent(t *testing.T) {
	withoutCache := FormatUsageSummary([]UsageModelTotals{
		{Model: "m", UsageTotals: UsageTotals{InputTokens: 1, OutputTokens: 1, Requests: 1}},
	})
	if strings.Contains(withoutCache, "cache") {
		t.Fatalf("expected no cache mention when cache tokens are zero, got %q", withoutCache)
	}
	withCache := FormatUsageSummary([]UsageModelTotals{
		{Model: "m", UsageTotals: UsageTotals{InputTokens: 1, OutputTokens: 1, CacheReadTokens: 5, CacheWriteTokens: 7, Requests: 1}},
	})
	if !strings.Contains(withCache, "5 cache read") || !strings.Contains(withCache, "7 cache write") {
		t.Fatalf("expected cache read/write to be reported, got %q", withCache)
	}
}

func TestFormatTokenCompact(t *testing.T) {
	cases := []struct {
		tokens int64
		want   string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1K"},
		{1234, "1.2K"},
		{999_999, "1000K"},
		{1_000_000, "1M"},
		{1_234_000, "1.2M"},
	}
	for _, tc := range cases {
		if got := formatTokenCompact(tc.tokens); got != tc.want {
			t.Errorf("formatTokenCompact(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
}
