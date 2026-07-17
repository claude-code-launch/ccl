package oauthproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverCompletedOnlyTextInjectsCreatedBeforeContent(t *testing.T) {
	t.Parallel()

	// Upstream skips response.created and jumps straight to a text delta.
	source := io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_late","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`,
		``,
		``,
	}, "\n")))
	reader, writer := io.Pipe()
	go recoverCompletedOnlyText(source, writer)

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read recovered stream: %v", err)
	}
	out := string(body)
	createdIdx := strings.Index(out, `"type":"response.created"`)
	deltaIdx := strings.Index(out, `"type":"response.output_text.delta"`)
	if createdIdx < 0 || deltaIdx < 0 {
		t.Fatalf("missing created/delta in recovered stream: %s", out)
	}
	if createdIdx > deltaIdx {
		t.Fatalf("response.created must precede content deltas:\n%s", out)
	}
}

func TestRecoverCompletedOnlyTextDropsLateRealCreated(t *testing.T) {
	t.Parallel()

	// Content first forces a synthetic created; a later real created must not
	// produce a second message_start-equivalent frame for CLIProxyAPI.
	source := io.NopCloser(strings.NewReader(strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"early"}`,
		``,
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_late","model":"gpt-test","status":"in_progress"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_late","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"early"}]}]}}`,
		``,
		``,
	}, "\n")))
	reader, writer := io.Pipe()
	go recoverCompletedOnlyText(source, writer)

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read recovered stream: %v", err)
	}
	out := string(body)
	if count := strings.Count(out, `"type":"response.created"`); count != 1 {
		t.Fatalf("response.created count = %d, want 1:\n%s", count, out)
	}
	// Late real created used event: response.created — that event line must be gone.
	if strings.Contains(out, "event: response.created") {
		t.Fatalf("late real response.created event line was forwarded:\n%s", out)
	}
	// Synthetic created is data-only with resp_synthetic; completed may still mention resp_late.
	if !strings.Contains(out, `"id":"resp_synthetic"`) {
		t.Fatalf("expected synthetic created id:\n%s", out)
	}
	createdIdx := strings.Index(out, `"type":"response.created"`)
	deltaIdx := strings.Index(out, `"type":"response.output_text.delta"`)
	if createdIdx < 0 || deltaIdx < 0 || createdIdx > deltaIdx {
		t.Fatalf("created must precede content:\n%s", out)
	}
	if !strings.Contains(out, "early") {
		t.Fatalf("missing content: %s", out)
	}
}

func TestRecoverCompletedOnlyTextCompletedOnly(t *testing.T) {
	t.Parallel()

	source := io.NopCloser(strings.NewReader(
		`data: {"type":"response.completed","response":{"id":"resp_compact","model":"gpt-test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact summary"}]}]}}` + "\n\n",
	))
	reader, writer := io.Pipe()
	go recoverCompletedOnlyText(source, writer)

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read recovered stream: %v", err)
	}
	out := string(body)
	createdIdx := strings.Index(out, `"type":"response.created"`)
	deltaIdx := strings.Index(out, `"type":"response.output_text.delta"`)
	completedIdx := strings.Index(out, `"type":"response.completed"`)
	if createdIdx < 0 || deltaIdx < 0 || completedIdx < 0 {
		t.Fatalf("missing events in recovered stream: %s", out)
	}
	if !(createdIdx < deltaIdx && deltaIdx < completedIdx) {
		t.Fatalf("want created < delta < completed:\n%s", out)
	}
	if !strings.Contains(out, "compact summary") {
		t.Fatalf("missing recovered text: %s", out)
	}
}

func TestPlainResponsesProxyStripsCodexResidue(t *testing.T) {
	t.Parallel()

	var sawClientMetadata bool
	var sawOriginator bool
	var sawSession bool
	var sawCodexUA bool
	var userAgent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "client_metadata") {
			sawClientMetadata = true
		}
		if r.Header.Get("Originator") != "" {
			sawOriginator = true
		}
		for key := range r.Header {
			normalized := strings.ReplaceAll(strings.ToLower(key), "_", "-")
			if normalized == "session-id" || normalized == "thread-id" || normalized == "x-codex-window-id" {
				sawSession = true
			}
		}
		userAgent = r.Header.Get("User-Agent")
		if strings.Contains(userAgent, "codex") {
			sawCodexUA = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[]}`))
	}))
	t.Cleanup(upstream.Close)

	compat, err := startResponsesCompatibilityProxy(upstream.URL, nil)
	if err != nil {
		t.Fatalf("startResponsesCompatibilityProxy: %v", err)
	}
	t.Cleanup(compat.Stop)

	// Simulate residual Codex headers that CLIProxyAPI injects for codex-api-key.
	req, err := http.NewRequest(http.MethodPost, compat.endpoint+"/responses", strings.NewReader(
		`{"model":"gpt-test","input":"hi","stream":false,"client_metadata":{"session_id":"x"},"prompt_cache_key":"x"}`,
	))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10")
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Session_id", "sess-1")
	req.Header.Set("Session-Id", "sess-1")
	req.Header.Set("Thread-Id", "sess-1")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if sawClientMetadata {
		t.Fatal("plain Responses proxy must strip client_metadata")
	}
	if sawOriginator {
		t.Fatal("plain Responses proxy must strip Originator")
	}
	if sawSession {
		t.Fatal("plain Responses proxy must strip Codex session headers")
	}
	if sawCodexUA {
		t.Fatalf("plain Responses proxy must replace Codex User-Agent, got %q", userAgent)
	}
	if userAgent != plainResponsesUserAgent {
		t.Fatalf("User-Agent = %q, want %q", userAgent, plainResponsesUserAgent)
	}
}
