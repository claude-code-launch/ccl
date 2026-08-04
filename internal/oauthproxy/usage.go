package oauthproxy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	cpausage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// UsageTotals accumulates token counts for one model across a session.
type UsageTotals struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Requests         int
}

// TokenTotal is InputTokens+OutputTokens, the number most reports lead with.
// Cache tokens are tracked separately: they are billed at a different rate and
// folding them in would make the total look larger than what was actually
// generated.
func (t UsageTotals) TokenTotal() int64 { return t.InputTokens + t.OutputTokens }

// UsageTracker accumulates per-model token usage for a single ccl session.
//
// One instance is shared by every runtime a session starts (a provider can run
// more than one backend, e.g. plain Responses fronted by the compatibility
// proxy), and it is safe for concurrent use because a streaming response and a
// retry can report on different goroutines.
type UsageTracker struct {
	mu      sync.Mutex
	byModel map[string]*UsageTotals
	order   []string
}

// NewUsageTracker returns an empty tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{byModel: make(map[string]*UsageTotals)}
}

// Add records one request's usage against a model. An empty model name is
// recorded as "unknown" rather than silently discarded, so a gap in the
// underlying protocol's usage reporting is visible instead of invisible.
func (u *UsageTracker) Add(model string, input, output, cacheRead, cacheWrite int64) {
	if u == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "unknown"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	totals, ok := u.byModel[model]
	if !ok {
		totals = &UsageTotals{}
		u.byModel[model] = totals
		u.order = append(u.order, model)
	}
	totals.InputTokens += input
	totals.OutputTokens += output
	totals.CacheReadTokens += cacheRead
	totals.CacheWriteTokens += cacheWrite
	totals.Requests++
}

// Snapshot returns the accumulated totals ordered by first use, and whether
// anything was recorded at all.
func (u *UsageTracker) Snapshot() ([]UsageModelTotals, bool) {
	if u == nil {
		return nil, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.order) == 0 {
		return nil, false
	}
	out := make([]UsageModelTotals, 0, len(u.order))
	for _, model := range u.order {
		out = append(out, UsageModelTotals{Model: model, UsageTotals: *u.byModel[model]})
	}
	return out, true
}

// UsageModelTotals pairs a model name with its accumulated totals.
type UsageModelTotals struct {
	Model string
	UsageTotals
}

// cpaUsagePlugin adapts a UsageTracker to CLIProxyAPI's usage.Plugin interface.
//
// CLIProxyAPI's executors publish one usage.Record per upstream request/stream
// regardless of protocol (openai_chat, openai_responses, codex, or any OAuth
// backend it drives), through the same global manager Runtime.RegisterUsagePlugin
// hooks into. This is the only place that sees every one of those backends; the
// alternative would be intercepting each protocol's response body ourselves.
type cpaUsagePlugin struct {
	tracker *UsageTracker
}

// HandleUsage implements usage.Plugin. Failed requests are not counted: a 401 or
// a rate limit carries no real token cost and would otherwise inflate totals
// with zero-token entries.
func (p cpaUsagePlugin) HandleUsage(_ context.Context, record cpausage.Record) {
	if record.Failed {
		return
	}
	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = record.Model
	}
	p.tracker.Add(model,
		record.Detail.InputTokens,
		record.Detail.OutputTokens,
		record.Detail.CacheReadTokens,
		record.Detail.CacheCreationTokens,
	)
}

// FormatUsageSummary renders one line per model plus a total line, in the style
// of the existing "[ccl log] session ended" line: a single fixed prefix,
// printed unconditionally rather than gated behind the debug toggle, because
// this is usage information for the user, not a diagnostic.
//
// Models are sorted by total tokens (input+output), largest first, so the model
// that mattered most for cost is the first thing printed.
func FormatUsageSummary(totals []UsageModelTotals) string {
	if len(totals) == 0 {
		return ""
	}
	sorted := make([]UsageModelTotals, len(totals))
	copy(sorted, totals)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TokenTotal() > sorted[j].TokenTotal()
	})

	var grandInput, grandOutput, grandCacheRead, grandCacheWrite int64
	var grandRequests int
	var lines []string
	for _, entry := range sorted {
		grandInput += entry.InputTokens
		grandOutput += entry.OutputTokens
		grandCacheRead += entry.CacheReadTokens
		grandCacheWrite += entry.CacheWriteTokens
		grandRequests += entry.Requests
		lines = append(lines, "[ccl usage] "+formatUsageLine(entry.Model, entry.UsageTotals))
	}
	if len(sorted) > 1 {
		lines = append(lines, "[ccl usage] "+formatUsageLine("total", UsageTotals{
			InputTokens:      grandInput,
			OutputTokens:     grandOutput,
			CacheReadTokens:  grandCacheRead,
			CacheWriteTokens: grandCacheWrite,
			Requests:         grandRequests,
		}))
	}
	return strings.Join(lines, "\n")
}

func formatUsageLine(model string, totals UsageTotals) string {
	parts := []string{
		fmt.Sprintf("%s in", formatTokenCompact(totals.InputTokens)),
		fmt.Sprintf("%s out", formatTokenCompact(totals.OutputTokens)),
	}
	if totals.CacheReadTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s cache read", formatTokenCompact(totals.CacheReadTokens)))
	}
	if totals.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s cache write", formatTokenCompact(totals.CacheWriteTokens)))
	}
	return fmt.Sprintf("%s: %s (%d req)", model, strings.Join(parts, " · "), totals.Requests)
}

// formatTokenCompact renders a token count the way usage dashboards do:
// thousands as "12.3K", millions as "1.2M", anything smaller as-is.
func formatTokenCompact(tokens int64) string {
	switch {
	case tokens >= 1_000_000:
		return trimTrailingZero(float64(tokens)/1_000_000) + "M"
	case tokens >= 1_000:
		return trimTrailingZero(float64(tokens)/1_000) + "K"
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func trimTrailingZero(value float64) string {
	s := fmt.Sprintf("%.1f", value)
	return strings.TrimSuffix(strings.TrimSuffix(s, "0"), ".")
}
