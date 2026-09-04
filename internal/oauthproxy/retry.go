package oauthproxy

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// upstreamFastRetryBackoff is the wait before each fast retry of a 429/5xx
// upstream outcome, shared by every CCL data plane. Three attempts total: the
// initial one plus one retry per entry. Upstream Retry-After hints are
// deliberately not honored inside this loop — they are routinely 30s+, which
// is slower than handing the failure back to Claude Code, whose own backoff
// reads the relayed Retry-After. The final failure relays the original
// response byte-for-byte, so the client sees exactly what it would have seen
// without this loop.
// Var (not const) so tests can shorten it; no test in this package runs in
// parallel.
var upstreamFastRetryBackoff = []time.Duration{500 * time.Millisecond, time.Second}

// upstreamStatusError is implemented by each runtime's typed upstream error so
// retryUpstream can branch on the upstream status without knowing the runtime.
// Kiro does not implement it: its own callUpstream loop is its retry layer.
type upstreamStatusError interface {
	upstreamStatus() int
}

// isFastRetryStatus reports whether an upstream HTTP status gets the fast
// retry treatment. 401/403 are excluded on purpose: each runtime refreshes
// OAuth on its own inside the attempt closure, and a refreshed 401 is terminal.
func isFastRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// retryUpstream runs attempt until it yields a non-retryable outcome or the
// fast retry budget is spent. A retryable outcome is a response with a 429/5xx
// status or an error carrying one. The LAST outcome is returned unchanged —
// response undrained, error as-is — so the caller's relay (status, body,
// Retry-After) is byte-for-byte what it was before this loop existed.
func retryUpstream(ctx context.Context, component string, attempt func() (*http.Response, error)) (*http.Response, error) {
	backoff := upstreamFastRetryBackoff
	for index := 0; ; index++ {
		response, err := attempt()
		if !retryableUpstreamOutcome(response, err) || index >= len(backoff) {
			return response, err
		}
		delay := backoff[index]
		LogWarnEvent("upstream_retry", "component", component, "request_id", requestLogID(ctx),
			"status", outcomeStatus(response, err), "retry", index+1, "retry_limit", len(backoff),
			"wait", delay, "action", "fast_retry")
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			// The client gave up mid-backoff: hand back the untouched outcome —
			// the original 429/5xx, not the cancellation.
			return response, err
		}
		drainAndClose(response)
	}
}

func retryableUpstreamOutcome(response *http.Response, err error) bool {
	return isFastRetryStatus(outcomeStatus(response, err))
}

// outcomeStatus reads the upstream status from either outcome shape: raw
// responses (passthrough/gemini return non-2xx as responses) or the typed
// errors the converter runtimes drain into.
func outcomeStatus(response *http.Response, err error) int {
	if response != nil {
		return response.StatusCode
	}
	var statusErr upstreamStatusError
	if err != nil && errors.As(err, &statusErr) {
		return statusErr.upstreamStatus()
	}
	return 0
}

// sleepContext waits for delay or until ctx is cancelled, whichever comes
// first. Retries and other waits must go through it so a client abort stops
// the wait instead of running out the clock.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
