package webui

import (
	"encoding/json"
	"strings"
	"testing"
)

type recorded struct {
	kind    string
	payload any
}

func translate(t *testing.T, lines ...string) []recorded {
	t.Helper()
	var events []recorded
	translator := newTranslator()
	for _, line := range lines {
		translator.Handle([]byte(line), func(kind string, payload any) {
			events = append(events, recorded{kind: kind, payload: payload})
		})
	}
	return events
}

func findKind(events []recorded, kind string) []recorded {
	var out []recorded
	for _, event := range events {
		if event.kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func TestTranslatorInitAndPartialText(t *testing.T) {
	events := translate(t,
		`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5-5","cwd":"/w"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`,
	)
	inits := findKind(events, EventInit)
	if len(inits) != 1 {
		t.Fatalf("init events = %d", len(inits))
	}
	init := inits[0].payload.(InitPayload)
	if init.SessionID != "sess-1" || init.Model != "claude-opus-5-5" || init.Cwd != "/w" {
		t.Fatalf("init payload = %+v", init)
	}
	tokens := findKind(events, EventToken)
	if len(tokens) != 2 {
		t.Fatalf("token events = %d", len(tokens))
	}
	if text := tokens[0].payload.(TokenPayload).Text + tokens[1].payload.(TokenPayload).Text; text != "Hello" {
		t.Fatalf("streamed text = %q", text)
	}
}

func TestTranslatorToolUseAndResult(t *testing.T) {
	events := translate(t,
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_1","name":"Bash"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./...","description":"run tests"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","is_error":false}]}}`,
	)
	uses := findKind(events, EventToolUse)
	if len(uses) != 2 {
		t.Fatalf("tool_use events = %d, want start + assistant", len(uses))
	}
	summary := uses[1].payload.(ToolUsePayload).Summary
	if summary != "go test ./..." {
		t.Fatalf("summary = %q", summary)
	}
	results := findKind(events, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("tool_result events = %d", len(results))
	}
	result := results[0].payload.(ToolResultPayload)
	if result.ID != "toolu_1" || result.Name != "Bash" || !result.OK || result.Output != "ok" {
		t.Fatalf("result = %+v", result)
	}
}

func TestTranslatorBlockArrayToolResult(t *testing.T) {
	events := translate(t,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`,
	)
	results := findKind(events, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	result := results[0].payload.(ToolResultPayload)
	if result.OK || result.Output != "line one\nline two" {
		t.Fatalf("result = %+v", result)
	}
}

func TestTranslatorSubagentFlag(t *testing.T) {
	events := translate(t,
		`{"type":"stream_event","parent_tool_use_id":"toolu_parent","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sub"}}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file_path":"/w/a.go"}}]}}`,
	)
	tokens := findKind(events, EventToken)
	if len(tokens) != 1 || !tokens[0].payload.(TokenPayload).Subagent {
		t.Fatalf("subagent token payload = %+v", tokens)
	}
	uses := findKind(events, EventToolUse)
	if len(uses) != 1 || !uses[0].payload.(ToolUsePayload).Subagent {
		t.Fatalf("subagent tool payload = %+v", uses)
	}
}

func TestTranslatorResultUsage(t *testing.T) {
	events := translate(t,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`,
	)
	results := findKind(events, EventResult)
	if len(results) != 1 {
		t.Fatalf("result events = %d", len(results))
	}
	payload := results[0].payload.(ResultPayload)
	if payload.IsError || payload.Message != "" {
		t.Fatalf("result payload = %+v", payload)
	}
	if payload.Usage.InputTokens != 17 || payload.Usage.OutputTokens != 20 || payload.Usage.DurationMs != 1234 || payload.Usage.CostUSD != 0.05 {
		t.Fatalf("usage = %+v", payload.Usage)
	}
}

func TestTranslatorErrorResultRedacts(t *testing.T) {
	events := translate(t,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Authorization: Bearer sk-ant-abcdefghijklmnop failed"}`,
	)
	results := findKind(events, EventResult)
	if len(results) != 1 {
		t.Fatalf("result events = %d", len(results))
	}
	payload := results[0].payload.(ResultPayload)
	if !payload.IsError || strings.Contains(payload.Message, "sk-ant-abcdefghijklmnop") {
		t.Fatalf("unredacted error message = %q", payload.Message)
	}
}

func TestTranslatorIgnoresNoise(t *testing.T) {
	events := translate(t,
		``,
		`not json`,
		`{"type":"system","subtype":"other"}`,
		`{"broken":`,
		`{"type":"unknown_type"}`,
	)
	if len(events) != 0 {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		input   string
		mustNot string
	}{
		{"curl -H 'Authorization: Bearer abcdefghijklmnop'", "abcdefghijklmnop"},
		{"export ANTHROPIC_API_KEY=sk-ant-1234567890", "sk-ant-1234567890"},
		{"OPENAI_API_KEY: sk-proj-xyz12345", "sk-proj-xyz12345"},
		{"password='hunter2hunter2'", "hunter2hunter2"},
	}
	for _, test := range cases {
		got := redactSecrets(test.input)
		if strings.Contains(got, test.mustNot) {
			t.Errorf("redactSecrets(%q) = %q, still contains the secret", test.input, got)
		}
	}
}

func TestSummarizeToolInputRedactsKeys(t *testing.T) {
	input := json.RawMessage(`{"path":"/w","apiKey":"sk-secret-value","nested":{"token":"abc"}}`)
	summary := summarizeToolInput("Read", input)
	if summary != "/w" {
		t.Fatalf("summary = %q", summary)
	}
	only := summarizeToolInput("Custom", json.RawMessage(`{"args":{"api_key":"sk-ant-secret","file":"a"},"name":"x"}`))
	if strings.Contains(only, "sk-ant-secret") {
		t.Fatalf("summarizeToolInput leaked a key: %q", only)
	}
}

func TestTruncateLines(t *testing.T) {
	text := strings.Join([]string{"1", "2", "3", "4"}, "\n")
	got := truncateLines(text, 2)
	if got != "1\n2\n…[truncated]" {
		t.Fatalf("truncateLines = %q", got)
	}
	if short := truncateLines("only", 2); short != "only" {
		t.Fatalf("truncateLines short = %q", short)
	}
}

func TestToolResultTruncatesOutput(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"`)
	for i := 0; i < ToolOutputLineLimit+10; i++ {
		if i > 0 {
			builder.WriteString(`\n`)
		}
		builder.WriteString("line")
	}
	builder.WriteString(`"}]}}`)
	events := translate(t, builder.String())
	results := findKind(events, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	output := results[0].payload.(ToolResultPayload).Output
	if !strings.HasSuffix(output, "…[truncated]") {
		t.Fatalf("output was not truncated (len %d)", len(output))
	}
	if lines := strings.Count(output, "\n"); lines != ToolOutputLineLimit {
		t.Fatalf("kept %d newlines, want %d", lines, ToolOutputLineLimit)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcdef", 3); got != "abc…" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("ab", 3); got != "ab" {
		t.Fatalf("truncateRunes short = %q", got)
	}
	if got := truncateRunes("汉字测试", 2); got != "汉字…" {
		t.Fatalf("truncateRunes runes = %q", got)
	}
}

func TestTranslatorToolDescription(t *testing.T) {
	var got ToolUsePayload
	newTranslator().Handle([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"git status","description":"Check the working tree"}}]}}`),
		func(kind string, payload any) { got = payload.(ToolUsePayload) })
	if got.Summary != "git status" || got.Description != "Check the working tree" {
		t.Fatalf("tool_use = %+v", got)
	}
}

func TestTranslatorForwardsRedactedToolInput(t *testing.T) {
	long := strings.Repeat("x", ToolInputStringLimit+50)
	var got ToolUsePayload
	newTranslator().Handle([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"a.go","old_string":"a","new_string":"`+long+`","api_key":"sk-live-123","nested":{"token":"abc"}}}]}}`),
		func(kind string, payload any) { got = payload.(ToolUsePayload) })
	var input map[string]any
	if err := json.Unmarshal(got.Input, &input); err != nil {
		t.Fatalf("input = %s: %v", got.Input, err)
	}
	if input["file_path"] != "a.go" || input["old_string"] != "a" {
		t.Fatalf("input = %v", input)
	}
	if input["api_key"] != "***" || input["nested"].(map[string]any)["token"] != "***" {
		t.Fatalf("credentials were forwarded: %v", input)
	}
	if n := len([]rune(input["new_string"].(string))); n != ToolInputStringLimit+1 {
		t.Fatalf("long string kept %d runes", n)
	}
}

func collectEvents(lines ...string) []struct {
	kind    string
	payload any
} {
	tr := newTranslator()
	var out []struct {
		kind    string
		payload any
	}
	for _, line := range lines {
		tr.Handle([]byte(line), func(kind string, payload any) {
			out = append(out, struct {
				kind    string
				payload any
			}{kind, payload})
		})
	}
	return out
}

func TestTranslatorDoesNotRepeatStreamedText(t *testing.T) {
	events := collectEvents(
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"cache_read_input_tokens":1000}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi"}]}}`,
	)
	if len(events) != 3 || events[0].kind != EventContext || events[1].kind != EventThinking || events[2].kind != EventToken {
		t.Fatalf("events = %+v", events)
	}
	if ctx := events[0].payload.(ContextPayload); ctx.Tokens != 1003 {
		t.Fatalf("context = %+v", ctx)
	}
}

func TestTranslatorEmitsUnstreamedSubagentText(t *testing.T) {
	events := collectEvents(
		`{"type":"assistant","parent_tool_use_id":"task_1","message":{"id":"msg_sub","content":[{"type":"text","text":"sub says"},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"a"}}]}}`,
	)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	token, ok := events[0].payload.(TokenPayload)
	if !ok || token.Text != "sub says" || token.Parent != "task_1" || !token.Subagent {
		t.Fatalf("token = %+v", events[0].payload)
	}
	if use := events[1].payload.(ToolUsePayload); use.Parent != "task_1" {
		t.Fatalf("tool_use = %+v", use)
	}
}
