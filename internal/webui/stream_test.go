package webui

import (
	"encoding/json"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

type recorded struct {
	kind    string
	payload any
}

func translateACP(t *testing.T, updates ...acp.SessionUpdate) (*acpTranslator, []recorded) {
	t.Helper()
	var events []recorded
	translator := newACPTranslator()
	for _, update := range updates {
		translator.Handle(update, func(kind string, payload any) {
			events = append(events, recorded{kind: kind, payload: payload})
		})
	}
	return translator, events
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

func TestTranslatorStreamingThoughtAndMessage(t *testing.T) {
	_, events := translateACP(t,
		acp.UpdateAgentThoughtText("let me "),
		acp.UpdateAgentThoughtText("think"),
		acp.UpdateAgentMessageText("Hel"),
		acp.UpdateAgentMessageText("lo"),
	)
	thoughts := findKind(events, EventThinking)
	if len(thoughts) != 2 {
		t.Fatalf("thinking events = %d", len(thoughts))
	}
	if text := thoughts[0].payload.(TokenPayload).Text + thoughts[1].payload.(TokenPayload).Text; text != "let me think" {
		t.Fatalf("streamed thought = %q", text)
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
	completed := acp.ToolCallStatusCompleted
	_, events := translateACP(t,
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "toolu_1",
				Kind:       acp.ToolKindExecute,
				Title:      "Run unit tests",
				RawInput:   map[string]any{"command": "go test ./...", "description": "Check unit tests"},
				Status:     acp.ToolCallStatusInProgress,
			},
		},
		acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{
				ToolCallId: "toolu_1",
				Status:     &completed,
				Content: []acp.ToolCallContent{
					acp.ToolContent(acp.TextBlock("line one")),
					acp.ToolContent(acp.TextBlock("line two")),
				},
			},
		},
	)
	uses := findKind(events, EventToolUse)
	if len(uses) != 1 {
		t.Fatalf("tool_use events = %d, want 1", len(uses))
	}
	use := uses[0].payload.(ToolUsePayload)
	if use.ID != "toolu_1" || use.Name != "Bash" || use.Summary != "go test ./..." || use.Description != "Check unit tests" {
		t.Fatalf("tool_use = %+v", use)
	}
	results := findKind(events, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("tool_result events = %d", len(results))
	}
	result := results[0].payload.(ToolResultPayload)
	if result.ID != "toolu_1" || result.Name != "Bash" || !result.OK || result.Output != "line one\nline two" {
		t.Fatalf("result = %+v", result)
	}
}

func TestTranslatorDiffToolsSingleAndMulti(t *testing.T) {
	completed := acp.ToolCallStatusCompleted
	oldText := "old line\n"
	_, events := translateACP(t,
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "write_1",
				Kind:       acp.ToolKindEdit,
				Title:      "Create b.go",
				Content:    []acp.ToolCallContent{acp.ToolDiffContent("b.go", "package main\n")},
				Status:     completed,
			},
		},
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "edit_1",
				Kind:       acp.ToolKindEdit,
				Title:      "Update a.go",
				Content:    []acp.ToolCallContent{acp.ToolDiffContent("a.go", "new line\n", oldText)},
				Status:     completed,
			},
		},
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "multi_1",
				Kind:       acp.ToolKindEdit,
				Title:      "Update two files",
				Content: []acp.ToolCallContent{
					acp.ToolDiffContent("x.go", "x2\n", "x1\n"),
					acp.ToolDiffContent("y.go", "y2\n"),
				},
				Status: completed,
			},
		},
	)
	uses := findKind(events, EventToolUse)
	if len(uses) != 3 {
		t.Fatalf("uses = %d, want 3", len(uses))
	}
	if uses[0].payload.(ToolUsePayload).Name != "Write" {
		t.Fatalf("single new diff = %+v", uses[0].payload)
	}
	if uses[1].payload.(ToolUsePayload).Name != "Edit" {
		t.Fatalf("single edit diff = %+v", uses[1].payload)
	}
	multi := uses[2].payload.(ToolUsePayload)
	if multi.Name != "MultiEdit" {
		t.Fatalf("multi diff = %+v", multi)
	}
	var multiInput struct {
		Diffs []struct {
			Path   string `json:"path"`
			Before string `json:"before"`
			After  string `json:"after"`
		} `json:"diffs"`
	}
	if err := json.Unmarshal(multi.Input, &multiInput); err != nil || len(multiInput.Diffs) != 2 || multiInput.Diffs[0].Path != "x.go" || multiInput.Diffs[1].Path != "y.go" {
		t.Fatalf("multi input = %s (%v)", multi.Input, err)
	}
}

func TestTranslatorPlanEmitsTodoWrite(t *testing.T) {
	_, events := translateACP(t, acp.UpdatePlan(
		acp.PlanEntry{Content: "Inspect code", Status: acp.PlanEntryStatusCompleted, Priority: acp.PlanEntryPriorityHigh},
		acp.PlanEntry{Content: "Run tests", Status: acp.PlanEntryStatusInProgress, Priority: acp.PlanEntryPriorityMedium},
	))
	uses := findKind(events, EventToolUse)
	results := findKind(events, EventToolResult)
	if len(uses) != 1 || len(results) != 1 {
		t.Fatalf("plan events = %+v", events)
	}
	use := uses[0].payload.(ToolUsePayload)
	if use.Name != "TodoWrite" || use.Summary != "2 tasks" || !strings.Contains(string(use.Input), `"status":"in_progress"`) {
		t.Fatalf("plan tool_use = %+v (%s)", use, use.Input)
	}
}

func TestTranslatorUsageUpdateTracksContextAndCost(t *testing.T) {
	tr, events := translateACP(t, acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{
			Used: 42500,
			Size: 200000,
			Cost: &acp.Cost{Amount: 0.125, Currency: "USD"},
		},
	})
	contexts := findKind(events, EventContext)
	if len(contexts) != 1 || contexts[0].payload.(ContextPayload).Tokens != 42500 {
		t.Fatalf("context = %+v", contexts)
	}
	if tr.SessionCostUSD() != 0.125 {
		t.Fatalf("cost = %v, want 0.125", tr.SessionCostUSD())
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		input   string
		mustNot string
	}{
		{"curl -H 'Authorization: Bearer abcdefghijklmnop'", "abcdefghijklmnop"},
		{"export XAI_API_KEY=xai-secret-1234567890", "xai-secret-1234567890"},
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
	only := summarizeToolInput("Custom", json.RawMessage(`{"args":{"api_key":"sk-secret-key","file":"a"},"name":"x"}`))
	if strings.Contains(only, "sk-secret-key") {
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

func TestToolResultTruncatesAndRedactsOutput(t *testing.T) {
	failed := acp.ToolCallStatusFailed
	lines := make([]string, ToolOutputLineLimit+10)
	lines[0] = "Authorization: Bearer secret-bearer-token-value"
	for i := 1; i < len(lines); i++ {
		lines[i] = "line"
	}
	_, events := translateACP(t, acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "t1",
			Kind:       acp.ToolKindExecute,
			Title:      "fail",
			RawOutput:  strings.Join(lines, "\n"),
			Status:     failed,
		},
	})
	results := findKind(events, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	res := results[0].payload.(ToolResultPayload)
	if res.OK {
		t.Fatal("failed tool reported OK=true")
	}
	if strings.Contains(res.Output, "secret-bearer-token-value") {
		t.Fatalf("unredacted secret in tool result: %q", res.Output[:80])
	}
	if !strings.HasSuffix(res.Output, "…[truncated]") {
		t.Fatalf("output was not truncated (len %d)", len(res.Output))
	}
	if count := strings.Count(res.Output, "\n"); count != ToolOutputLineLimit {
		t.Fatalf("kept %d newlines, want %d", count, ToolOutputLineLimit)
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

func TestTranslatorForwardsRedactedToolInput(t *testing.T) {
	long := strings.Repeat("x", ToolInputStringLimit+50)
	_, events := translateACP(t, acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "t",
			Kind:       acp.ToolKindEdit,
			Title:      "Edit a.go",
			RawInput: map[string]any{
				"file_path":  "a.go",
				"old_string": "a",
				"new_string": long,
				"api_key":    "sk-live-123",
				"nested":     map[string]any{"token": "abc"},
			},
			Status: acp.ToolCallStatusInProgress,
		},
	})
	uses := findKind(events, EventToolUse)
	if len(uses) != 1 {
		t.Fatalf("uses = %d", len(uses))
	}
	got := uses[0].payload.(ToolUsePayload)
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

func TestTranslatorGrokNativeToolNamesAndSubagentLineage(t *testing.T) {
	completed := acp.ToolCallStatusCompleted
	_, events := translateACP(t,
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "task_1",
				Title:      "spawn_subagent",
				RawInput:   map[string]any{"description": "Audit auth", "subagent_type": "explore"},
				Status:     acp.ToolCallStatusInProgress,
			},
		},
		acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock("subagent scanning"),
				Meta:    map[string]any{"parentToolCallId": "task_1", "subagent": true},
			},
		},
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "child_1",
				Title:      "list_dir",
				RawInput:   map[string]any{"path": "internal/auth"},
				Meta:       map[string]any{"parentToolCallId": "task_1"},
				Status:     completed,
			},
		},
		acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{
				ToolCallId: "task_1",
				Status:     &completed,
				RawOutput:  "Auth looks solid",
			},
		},
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "skill_1",
				Title:      "skill",
				RawInput:   map[string]any{"skill": "verify"},
				Status:     completed,
			},
		},
		acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "plan_1",
				Kind:       acp.ToolKindSwitchMode,
				Title:      "exit_plan_mode",
				RawInput:   map[string]any{"planContent": "# Plan\n1. Test"},
				Status:     acp.ToolCallStatusInProgress,
			},
		},
	)

	tokens := findKind(events, EventToken)
	if len(tokens) != 1 || !tokens[0].payload.(TokenPayload).Subagent || tokens[0].payload.(TokenPayload).Parent != "task_1" {
		t.Fatalf("subagent token = %+v", tokens)
	}

	uses := findKind(events, EventToolUse)
	if len(uses) != 4 {
		t.Fatalf("tool_use count = %d, want 4", len(uses))
	}
	if u := uses[0].payload.(ToolUsePayload); u.Name != "Task" || u.Description != "Audit auth" {
		t.Fatalf("spawn_subagent tool_use = %+v", u)
	}
	if u := uses[1].payload.(ToolUsePayload); u.Name != "LS" || !u.Subagent || u.Parent != "task_1" || u.Summary != "internal/auth" {
		t.Fatalf("child list_dir tool_use = %+v", u)
	}
	if u := uses[2].payload.(ToolUsePayload); u.Name != "Skill" || u.Summary != "verify" {
		t.Fatalf("skill tool_use = %+v", u)
	}
	planUse := uses[3].payload.(ToolUsePayload)
	if planUse.Name != "ExitPlanMode" || !strings.Contains(string(planUse.Input), `"plan":"# Plan\n1. Test"`) {
		t.Fatalf("exit_plan_mode tool_use = %+v (%s)", planUse, planUse.Input)
	}

	tasks := findKind(events, EventTask)
	if len(tasks) != 3 {
		t.Fatalf("task events = %+v, want 3 (running, LastTool=LS, completed)", tasks)
	}
	if p := tasks[0].payload.(TaskPayload); p.Status != "running" || p.Agent != "explore" {
		t.Fatalf("task[0] = %+v", p)
	}
	if p := tasks[1].payload.(TaskPayload); p.LastTool != "LS" {
		t.Fatalf("task[1] = %+v", p)
	}
	if p := tasks[2].payload.(TaskPayload); p.Status != "completed" || p.Summary != "Auth looks solid" {
		t.Fatalf("task[2] = %+v", p)
	}
}

func TestTranslatorGrokParallelSubagentsPreserveTaskIdentityAndUsage(t *testing.T) {
	completed := acp.ToolCallStatusCompleted
	otherKind := acp.ToolKindOther
	xaiMeta := map[string]any{
		"x.ai/tool": map[string]any{
			"version":   1,
			"name":      "spawn_subagent",
			"kind":      "task",
			"namespace": "grok_build",
			"label":     "Subagent",
		},
		"subagentBackground": false,
	}

	var events []recorded
	translator := newACPTranslator()
	emit := func(kind string, payload any) {
		events = append(events, recorded{kind: kind, payload: payload})
	}
	handle := func(u acp.SessionUpdate) {
		translator.Handle(u, emit)
	}

	spawnTitle1 := "Check Beijing weather"
	spawnTitle2 := "Check Shanghai weather"
	spawnTitle3 := "HangzhouWeather"

	// 1. Parallel tool_call + immediate tool_call_update rewriting title to description & kind=other.
	handle(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: "call_1",
		Title:      "spawn_subagent",
		RawInput:   map[string]any{"prompt": "Beijing forecast", "description": spawnTitle1, "background": false},
		Meta:       xaiMeta,
		Status:     acp.ToolCallStatusPending,
	}})
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_1",
		Kind:       &otherKind,
		Title:      &spawnTitle1,
		RawInput:   map[string]any{"variant": "Task", "prompt": "Beijing forecast", "description": spawnTitle1, "run_in_background": false},
		Meta:       xaiMeta,
	}})

	handle(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: "call_2",
		Title:      "spawn_subagent",
		RawInput:   map[string]any{"prompt": "Shanghai forecast", "description": spawnTitle2, "background": false},
		Meta:       xaiMeta,
		Status:     acp.ToolCallStatusPending,
	}})
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_2",
		Kind:       &otherKind,
		Title:      &spawnTitle2,
		RawInput:   map[string]any{"variant": "Task", "prompt": "Shanghai forecast", "description": spawnTitle2, "run_in_background": false},
		Meta:       xaiMeta,
	}})

	handle(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: "call_3",
		Title:      "spawn_subagent",
		RawInput:   map[string]any{"prompt": "Hangzhou forecast", "description": spawnTitle3, "background": false},
		Meta:       xaiMeta,
		Status:     acp.ToolCallStatusPending,
	}})
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_3",
		Kind:       &otherKind,
		Title:      &spawnTitle3,
		RawInput:   map[string]any{"variant": "Task", "prompt": "Hangzhou forecast", "description": spawnTitle3, "run_in_background": false},
		Meta:       xaiMeta,
	}})

	// 2. Extension _x.ai/session/update subagent_spawned & out-of-order subagent_finished.
	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_spawned","subagent_id":"sub_1","child_session_id":"child_1","subagent_type":"general-purpose","description":"Check Beijing weather"}`), emit)
	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_spawned","subagent_id":"sub_2","child_session_id":"child_2","subagent_type":"general-purpose","description":"Check Shanghai weather"}`), emit)
	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_spawned","subagent_id":"sub_3","child_session_id":"child_3","subagent_type":"general-purpose","description":"HangzhouWeather"}`), emit)

	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_finished","subagent_id":"sub_3","child_session_id":"child_3","status":"completed","tool_calls":12,"duration_ms":55794,"tokens_used":18365}`), emit)
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_3",
		Status:     &completed,
		Content:    []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Hangzhou report"))},
		RawOutput:  map[string]any{"type": "SubagentCompleted", "subagent_id": "sub_3", "subagent_type": "general-purpose", "tool_calls": 12, "duration_ms": 55794, "output": "Hangzhou report"},
	}})

	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_finished","subagent_id":"sub_1","child_session_id":"child_1","status":"completed","tool_calls":32,"duration_ms":118631,"tokens_used":34436}`), emit)
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_1",
		Status:     &completed,
		RawOutput:  map[string]any{"type": "SubagentCompleted", "subagent_id": "sub_1", "subagent_type": "general-purpose", "tool_calls": 32, "duration_ms": 118631, "output": "Beijing report"},
	}})

	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_finished","subagent_id":"sub_2","child_session_id":"child_2","status":"completed","tool_calls":23,"duration_ms":96099,"tokens_used":27149}`), emit)
	handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_2",
		Status:     &completed,
		Content:    []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Shanghai report"))},
		RawOutput:  map[string]any{"type": "SubagentCompleted", "subagent_id": "sub_2", "subagent_type": "general-purpose", "tool_calls": 23, "duration_ms": 96099, "output": "Shanghai report"},
	}})

	for _, ev := range findKind(events, EventToolUse) {
		use := ev.payload.(ToolUsePayload)
		if use.Name != "Task" {
			t.Fatalf("tool_use %q lost canonical Task name: %+v", use.ID, use)
		}
	}

	resultsByID := map[string]ToolResultPayload{}
	for _, ev := range findKind(events, EventToolResult) {
		res := ev.payload.(ToolResultPayload)
		resultsByID[res.ID] = res
	}
	if resultsByID["call_1"].Output != "Beijing report" || resultsByID["call_1"].Name != "Task" {
		t.Fatalf("call_1 result = %+v", resultsByID["call_1"])
	}

	completedTasks := map[string]TaskPayload{}
	for _, ev := range findKind(events, EventTask) {
		task := ev.payload.(TaskPayload)
		if task.Status == "completed" {
			completedTasks[task.ID] = task
		}
	}
	if len(completedTasks) != 3 {
		t.Fatalf("completed tasks = %d, want 3 (%+v)", len(completedTasks), completedTasks)
	}
	if c1 := completedTasks["call_1"]; c1.Agent != "general-purpose" || c1.Usage == nil || c1.Usage.Tokens != 34436 || c1.Usage.ToolUses != 32 || c1.Usage.DurationMs != 118631 {
		t.Fatalf("call_1 completed task = %+v", c1)
	}
	if c2 := completedTasks["call_2"]; c2.Usage == nil || c2.Usage.Tokens != 27149 || c2.Usage.ToolUses != 23 || c2.Usage.DurationMs != 96099 {
		t.Fatalf("call_2 completed task = %+v", c2)
	}
	if c3 := completedTasks["call_3"]; c3.Usage == nil || c3.Usage.Tokens != 18365 || c3.Usage.ToolUses != 12 || c3.Usage.DurationMs != 55794 {
		t.Fatalf("call_3 completed task = %+v", c3)
	}
}

func TestTranslatorGrokBackgroundSubagentWaitsForExtensionFinish(t *testing.T) {
	completed := acp.ToolCallStatusCompleted
	var events []recorded
	translator := newACPTranslator()
	emit := func(kind string, payload any) {
		events = append(events, recorded{kind: kind, payload: payload})
	}

	translator.Handle(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: "call_bg",
		Title:      "spawn_subagent",
		RawInput:   map[string]any{"prompt": "Run background audit", "description": "Background audit", "background": true},
		Meta:       map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}, "subagentBackground": true},
		Status:     acp.ToolCallStatusPending,
	}}, emit)
	translator.Handle(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: "call_bg",
		Status:     &completed,
		RawOutput:  map[string]any{"type": "SubagentSpawned", "subagent_id": "sub_bg_1", "subagent_type": "explore"},
	}}, emit)

	for _, ev := range findKind(events, EventTask) {
		task := ev.payload.(TaskPayload)
		if task.Status != "running" {
			t.Fatalf("background spawn should keep task running, got %+v", task)
		}
	}

	translator.HandleExtensionUpdate(json.RawMessage(`{"sessionUpdate":"subagent_finished","subagent_id":"sub_bg_1","status":"completed","tool_calls":5,"duration_ms":8100,"tokens_used":4200,"output":"Background report"}`), emit)

	results := findKind(events, EventToolResult)
	if len(results) != 2 || results[1].payload.(ToolResultPayload).Output != "Background report" {
		t.Fatalf("tool results = %+v", results)
	}
	tasks := findKind(events, EventTask)
	last := tasks[len(tasks)-1].payload.(TaskPayload)
	if last.ID != "call_bg" || last.Status != "completed" || last.Agent != "explore" || last.Summary != "Background report" {
		t.Fatalf("final task = %+v", last)
	}
	if last.Usage == nil || last.Usage.Tokens != 4200 || last.Usage.ToolUses != 5 || last.Usage.DurationMs != 8100 {
		t.Fatalf("final task usage = %+v", last.Usage)
	}
}
