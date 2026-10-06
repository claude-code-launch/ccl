package oauthproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestFoldAnthropicStreamSkipsUnusableEvents pins the fold's tolerance: an
// upstream that interleaves keep-alives, unparseable data lines, and deltas for
// blocks it never started must still produce a usable message.
func TestFoldAnthropicStreamSkipsUnusableEvents(t *testing.T) {
	stream := strings.Join([]string{
		": keep-alive comment",
		"event: ping",
		"data: not json",
		"data: [DONE]",
		"",
		`data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"orphan"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"second"}}`,
		`data: {"type":"content_block_start","index":2,"content_block":null}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"why"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_stop","index":9}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		"",
	}, "\n")

	message, foldErr := foldAnthropicStream([]byte(stream))
	if foldErr != nil {
		t.Fatalf("fold error = %+v", foldErr)
	}
	if message["stop_reason"] != "tool_use" || message["usage"].(map[string]any)["output_tokens"] != float64(5) {
		t.Fatalf("message = %v", message)
	}
	blocks, _ := message["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content = %v, want two indexed blocks", blocks)
	}
	tool, _ := blocks[0].(map[string]any)
	if tool["name"] != "f" || tool["signature"] != "sig" || tool["thinking"] != "why" {
		t.Fatalf("tool block = %v", tool)
	}
	input, _ := tool["input"].(map[string]any)
	if input["a"] != float64(1) {
		t.Fatalf("streamed input_json was not assembled: %v", tool["input"])
	}
}

// TestFoldAnthropicStreamRequiresMessageStart covers a stream that only ever
// carries deltas: there is nothing to fold, which is an upstream error.
func TestFoldAnthropicStreamRequiresMessageStart(t *testing.T) {
	stream := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n"
	_, foldErr := foldAnthropicStream([]byte(stream))
	if foldErr == nil || !strings.Contains(foldErr.message, "before message_start") {
		t.Fatalf("fold error = %+v", foldErr)
	}
}

// TestFoldAnthropicStreamIgnoresDeltasAfterAnErrorEvent documents that an error
// event ends the fold immediately, so later events cannot resurrect a message.
func TestFoldAnthropicStreamIgnoresDeltasAfterAnErrorEvent(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","content":[],"usage":{}}}`,
		`data: {"type":"error","error":{"message":"boom"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	}, "\n")
	message, foldErr := foldAnthropicStream([]byte(stream))
	if message != nil || foldErr == nil || foldErr.errorType != "api_error" || foldErr.message != "boom" {
		t.Fatalf("fold = %v, %+v", message, foldErr)
	}
}

// TestZedRuntimeReportsAStreamThatCannotBeFolded covers the non-streaming
// client path when the upstream stream is unusable.
func TestZedRuntimeReportsAStreamThatCannotBeFolded(t *testing.T) {
	cloud := newFakeZedCloud(t)
	cloud.completion = func(call fakeZedCall, writer http.ResponseWriter) bool {
		writer.Header().Set(zedServerStatusHeader, "true")
		writer.Header().Set("Content-Type", "application/x-ndjson")
		// message_stop keeps the gateway happy, but no message_start ever
		// arrived, so there is nothing for the fold to return.
		_, _ = io.WriteString(writer, ndjson(
			`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"orphan"}}}`,
			`{"event":{"type":"message_stop"}}`,
			`{"status":"stream_ended"}`,
		))
		return true
	}
	runtime := startZedTestRuntime(t, cloud, "")

	response, body := zedPost(t, runtime, "/v1/messages", zedClaudeCodeRequest("claude-sonnet-test", false))
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.StatusCode, body)
	}
	if !strings.Contains(gjson.Get(body, "error.message").String(), "message_start") {
		t.Fatalf("body = %s", body)
	}
}
