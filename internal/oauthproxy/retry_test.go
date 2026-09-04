package oauthproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// swapFastRetryBackoff shortens the shared fast-retry backoff for one test so
// retried requests do not sleep in real time. It snapshots the previous slice
// in case another test swaps mid-run; no test in this package runs in
// parallel, matching the package-var swap idiom used elsewhere.
func swapFastRetryBackoff(t *testing.T, backoff []time.Duration) {
	t.Helper()
	previous := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = backoff
	t.Cleanup(func() {
		upstreamFastRetryBackoff = previous
	})
}

func staticFastRetryBackoff(t *testing.T) {
	t.Helper()
	swapFastRetryBackoff(t, []time.Duration{time.Millisecond, time.Millisecond})
}

func TestRetryUpstreamRecoversWithinBudget(t *testing.T) {
	staticFastRetryBackoff(t)
	attempts := 0
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return responseWithStatus(http.StatusTooManyRequests), nil
		}
		return responseWithStatus(http.StatusOK), nil
	})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryUpstreamExhaustsBudgetAndReturnsLastError(t *testing.T) {
	staticFastRetryBackoff(t)
	attempts := 0
	sentinel := errors.New("upstream exploded")
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return responseWithStatus(http.StatusInternalServerError), nil
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryUpstreamReturnsLastResponseUndrained(t *testing.T) {
	staticFastRetryBackoff(t)
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		// Fresh body each call; the final one must reach the caller open.
		return responseWithStatus(http.StatusServiceUnavailable), nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("final response body unreadable: %v", readErr)
	}
	if len(body) == 0 {
		t.Fatal("final response body drained before relay")
	}
	_ = response.Body.Close()
}

func TestRetryUpstreamIgnoresClientErrorStatuses(t *testing.T) {
	staticFastRetryBackoff(t)
	attempts := 0
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		return responseWithStatus(http.StatusBadRequest), nil
	})
	if err != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx is not fast-retryable)", attempts)
	}
}

func TestRetryUpstreamIgnoresTransportErrors(t *testing.T) {
	staticFastRetryBackoff(t)
	attempts := 0
	sentinel := errors.New("connection refused")
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) || response != nil {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (statusless transport errors are relayed)", attempts)
	}
}

func TestRetryUpstreamRetriesTypedErrorStatus(t *testing.T) {
	staticFastRetryBackoff(t)
	attempts := 0
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		if attempts < 2 {
			// Wrapped the way real runtimes surface upstream failures: %w keeps
			// the typed status visible to errors.As inside retryUpstream.
			return nil, fmt.Errorf("qoder call: %w", &qoderUpstreamError{status: http.StatusBadGateway, body: "boom"})
		}
		return responseWithStatus(http.StatusOK), nil
	})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRetryUpstreamAbandonsWaitOnContextCancel(t *testing.T) {
	// A real multi-second backoff: the only way this test finishes quickly is
	// the client cancelling mid-wait, which must return the untouched 429 —
	// not the context error, and not a second attempt after a full 30s sleep.
	swapFastRetryBackoff(t, []time.Duration{30 * time.Second})
	attempts := 0
	firstAttempt := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel as soon as the first attempt yields its 429 and the loop
		// enters the backoff sleep.
		<-firstAttempt
		cancel()
	}()
	started := time.Now()
	response, err := retryUpstream(ctx, "test", func() (*http.Response, error) {
		attempts++
		if attempts == 1 {
			close(firstAttempt)
		}
		return responseWithStatus(http.StatusTooManyRequests), nil
	})
	if err != nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("retryUpstream slept %v, want the cancellation to cut the backoff short", elapsed)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
}

func TestRetryUpstreamClosesRetriedResponses(t *testing.T) {
	staticFastRetryBackoff(t)
	var closed atomic.Int32
	attempts := 0
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		if attempts < 3 {
			response := responseWithStatus(http.StatusInternalServerError)
			// Track close via the body's Close.
			original := response.Body
			response.Body = &closeCounter{ReadCloser: original, closed: &closed}
			return response, nil
		}
		return responseWithStatus(http.StatusOK), nil
	})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if got := closed.Load(); got != 2 {
		t.Fatalf("retried responses closed = %d, want 2", got)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
}

type closeCounter struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (c *closeCounter) Close() error {
	c.closed.Add(1)
	return c.ReadCloser.Close()
}

// responseWithStatus builds a non-reused HTTP response with a non-empty body
// so undrained-relay assertions can tell drained from open bodies apart.
func responseWithStatus(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(newStaticReader("upstream payload")),
	}
}

type staticReader struct {
	content string
}

func newStaticReader(content string) *staticReader { return &staticReader{content: content} }

func (r *staticReader) Read(p []byte) (int, error) {
	if r.content == "" {
		return 0, io.EOF
	}
	n := copy(p, r.content)
	r.content = r.content[n:]
	return n, nil
}

func TestIsFastRetryStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
		{http.StatusPaymentRequired, false},
		{http.StatusOK, false},
		{0, false},
	}
	for _, test := range tests {
		if got := isFastRetryStatus(test.status); got != test.want {
			t.Fatalf("isFastRetryStatus(%d) = %v, want %v", test.status, got, test.want)
		}
	}
}

func TestRetryUpstreamSuccessSucceedsOnFirstTry(t *testing.T) {
	// Full-length backoff: no retry must happen, so the test must not sleep.
	attempts := 0
	response, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		attempts++
		return responseWithStatus(http.StatusOK), nil
	})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v err=%v", response, err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

// TestGeminiForwardFastRetryCoversBothBases pins the attempt arithmetic: ONE
// attempt walks daily→prod (2 upstream calls), so exhausting the budget is
// 3 attempts × 2 bases = 6 calls, in strict daily,prod order.
func TestGeminiForwardFastRetryCoversBothBases(t *testing.T) {
	staticFastRetryBackoff(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	service := &geminiService{
		authorizer: &chatStaticAuthorizer{token: "test-token"},
		client:     upstream.Client(),
	}
	// Redirect both bases at the stub so the fallback order stays observable.
	previousDaily, previousProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily = upstream.URL
	antigravityBaseURLProd = upstream.URL
	t.Cleanup(func() {
		antigravityBaseURLDaily, antigravityBaseURLProd = previousDaily, previousProd
	})

	response, err := service.forward(context.Background(), false, []byte("{}"))
	if err != nil {
		t.Fatalf("forward() error: %v", err)
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %v", response)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if got := calls.Load(); got != 6 {
		t.Fatalf("upstream calls = %d, want 6 (3 attempts × 2 bases)", got)
	}
}

func TestGeminiForwardFastRetryRecovers(t *testing.T) {
	staticFastRetryBackoff(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) >= 3 {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"candidates":[]}`))
			return
		}
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	service := &geminiService{
		authorizer: &chatStaticAuthorizer{token: "test-token"},
		client:     upstream.Client(),
	}
	previousDaily, previousProd := antigravityBaseURLDaily, antigravityBaseURLProd
	antigravityBaseURLDaily = upstream.URL
	antigravityBaseURLProd = upstream.URL
	t.Cleanup(func() {
		antigravityBaseURLDaily, antigravityBaseURLProd = previousDaily, previousProd
	})

	response, err := service.forward(context.Background(), false, []byte("{}"))
	if err != nil {
		t.Fatalf("forward() error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3 (2 failed bases then success)", got)
	}
}

func TestRetryUpstreamRetriesPostDataEveryAttempt(t *testing.T) {
	// A retried request must re-send the same payload: the closure owns the
	// body, so this pins that retryUpstream does not consume it.
	staticFastRetryBackoff(t)
	var bodies []string
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		bodies = append(bodies, string(raw))
		if attempts.Add(1) < 3 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	payload := []byte(`{"model":"m","messages":[]}`)
	_, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		request, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		response, doErr := upstream.Client().Do(request)
		if doErr != nil {
			return nil, doErr
		}
		if response.StatusCode != http.StatusOK {
			// %w keeps the typed upstream status visible to errors.As, matching
			// how the converter runtimes wrap their drain errors.
			return nil, fmt.Errorf("upstream call: %w", &qoderUpstreamError{status: response.StatusCode, body: "stub"})
		}
		return response, nil
	})
	if err != nil {
		t.Fatalf("retryUpstream() error: %v", err)
	}
	if len(bodies) != 3 || bodies[0] != string(payload) || bodies[2] != string(payload) {
		t.Fatalf("bodies = %q", bodies)
	}
}
