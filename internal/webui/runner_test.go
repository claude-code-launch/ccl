package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs(t *testing.T) {
	args := buildArgs(LaunchSpec{Prompt: "hello world"})
	want := []string{
		"--print", "--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", "bypassPermissions",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v", args)
	}
	// The prompt never travels in argv: it is part of the stdin message.
	line, err := stdinMessage(LaunchSpec{Prompt: "hello world"})
	if err != nil || !strings.Contains(string(line), `"text":"hello world"`) {
		t.Fatalf("stdin = %s (%v)", line, err)
	}
	// A mode that asks routes the questions to stdio.
	asking := strings.Join(buildArgs(LaunchSpec{Prompt: "p", PermissionMode: "default"}), " ")
	if !strings.Contains(asking, "--permission-mode default --permission-prompt-tool stdio") {
		t.Fatalf("asking args = %s", asking)
	}
}

func TestBuildArgsAddsModelEffortResume(t *testing.T) {
	args := buildArgs(LaunchSpec{Prompt: "p", Model: "opus", Effort: "high", Resume: "sess-1"})
	joined := strings.Join(args, "\x00")
	for _, fragment := range []string{"\x00--model\x00opus", "\x00--effort\x00high", "\x00--resume\x00sess-1"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("missing %q in %v", fragment, args)
		}
	}
	// Empty knobs are omitted rather than sent blank.
	plain := strings.Join(buildArgs(LaunchSpec{Prompt: "p"}), "\x00")
	if strings.Contains(plain, "--model") || strings.Contains(plain, "--resume") || strings.Contains(plain, "--effort") {
		t.Fatalf("empty spec added flags: %v", plain)
	}
}

// TestHelperProcess is not a test: the runner tests re-execute this binary so
// they get a real child process without depending on a shell.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CCL_WEBUI_HELPER") != "1" {
		t.Skip("helper process")
	}
	for _, line := range strings.Split(os.Getenv("CCL_WEBUI_HELPER_OUTPUT"), "\n") {
		if line != "" {
			os.Stdout.WriteString(line + "\n")
		}
	}
	if os.Getenv("CCL_WEBUI_HELPER_PERMISSION") == "1" {
		helperAskPermission()
		os.Exit(0)
	}
	if wait := os.Getenv("CCL_WEBUI_HELPER_SLEEP"); wait != "" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

// helperAskPermission behaves like Claude Code under
// --permission-prompt-tool stdio: it reads the user message, asks for a Bash
// call and reports the answer it got in its result.
func helperAskPermission() {
	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 1<<20), 8<<20)
	stdin.Scan() // the user message
	os.Stdout.WriteString(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"tu-1","input":{"questions":[{"question":"Which db?","options":[{"label":"pg"},{"label":"sqlite"}]}],"token":"hush"},"permission_suggestions":[{"type":"addRules","rules":[{"toolName":"AskUserQuestion"}],"behavior":"allow","destination":"localSettings"}]}}` + "\n")
	os.Stdout.WriteString(`{"type":"control_request","request_id":"req-2","request":{"subtype":"hook_callback","callback_id":"x"}}` + "\n")
	answers := []string{}
	for len(answers) < 2 && stdin.Scan() {
		answers = append(answers, stdin.Text())
	}
	encoded, _ := json.Marshal(strings.Join(answers, "\n"))
	os.Stdout.WriteString(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":` + string(encoded) + `}}}` + "\n")
	os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false}` + "\n")
	// Exit only once stdin is closed, as Claude Code does.
	for stdin.Scan() {
	}
}

func helperPrepare(t *testing.T, output string, sleep bool) LaunchFunc {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		cmd := exec.Command(executable, "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "CCL_WEBUI_HELPER=1", "CCL_WEBUI_HELPER_OUTPUT="+output)
		if sleep {
			cmd.Env = append(cmd.Env, "CCL_WEBUI_HELPER_SLEEP=1")
		}
		return cmd, func() {}, nil
	}
}

func newTestRunner(t *testing.T, prepare LaunchFunc) *Runner {
	t.Helper()
	runner := NewRunner(2)
	runner.Prepare = prepare
	return runner
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("turn did not finish")
	}
}

func TestRunnerStoresTurnEvents(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Provider: "default", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"claude-1","model":"m"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.01,"usage":{"input_tokens":2,"output_tokens":3}}`,
	}, "\n")
	runner := newTestRunner(t, helperPrepare(t, output, false))
	timeline := NewTimeline()

	handle, err := timeline.Start(runner, session, LaunchSpec{Provider: "default", Workspace: session.Meta().Workspace, Prompt: "hello"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)

	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Type)
	}
	joined := strings.Join(kinds, ",")
	for _, kind := range []string{EventUser, EventInit, EventToken, EventResult} {
		if !strings.Contains(joined, kind) {
			t.Fatalf("missing %s in %v", kind, kinds)
		}
	}
	meta := session.Meta()
	if meta.Turns != 1 {
		t.Fatalf("Turns = %d, want 1", meta.Turns)
	}
	if meta.ClaudeSessionID != "claude-1" {
		t.Fatalf("ClaudeSessionID = %q", meta.ClaudeSessionID)
	}
	if meta.Usage.InputTokens != 2 || meta.Usage.OutputTokens != 3 || meta.Usage.CostUSD != 0.01 {
		t.Fatalf("usage = %+v", meta.Usage)
	}
}

func TestRunnerTurnCostIsDifferenceOfRunningTotal(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewTimeline()
	// Claude Code reports the session's running cost; a resumed turn continues it.
	for _, cost := range []string{"0.015", "0.04"} {
		output := `{"type":"result","subtype":"success","duration_ms":10,"total_cost_usd":` + cost + `,"usage":{"input_tokens":5,"output_tokens":1}}`
		handle, err := timeline.Start(newTestRunner(t, helperPrepare(t, output, false)), session, LaunchSpec{Prompt: "p"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitDone(t, handle.Done)
	}
	results := resultPayloads(t, session)
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	second := results[1]
	if second.Usage.CostUSD != 0.04 || second.TurnUsage == nil || !nearly(second.TurnUsage.CostUSD, 0.025) {
		t.Fatalf("second result = %+v turn=%+v", second, second.TurnUsage)
	}
	meta := session.Meta()
	if !nearly(meta.Usage.CostUSD, 0.04) || meta.Usage.InputTokens != 10 || meta.Usage.DurationMs != 20 {
		t.Fatalf("conversation usage = %+v", meta.Usage)
	}
}

func TestMetaRecordTurnIgnoresZeroedAndRestartedTotals(t *testing.T) {
	meta := Meta{}
	meta.recordTurn(Usage{CostUSD: 0.02})
	// A crash result carries zeros: nothing was spent, the total is kept.
	if turn := meta.recordTurn(Usage{}); turn.CostUSD != 0 || meta.ClaudeCostUSD != 0.02 {
		t.Fatalf("zeroed result: turn=%+v meta=%+v", turn, meta)
	}
	// A smaller non-zero total means a new running total started.
	if turn := meta.recordTurn(Usage{CostUSD: 0.005}); turn.CostUSD != 0.005 || !nearly(meta.Usage.CostUSD, 0.025) {
		t.Fatalf("restarted total: turn=%+v meta=%+v", turn, meta)
	}
}

func resultPayloads(t *testing.T, session *Session) []ResultPayload {
	t.Helper()
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var results []ResultPayload
	for _, event := range events {
		if event.Type != EventResult {
			continue
		}
		var result ResultPayload
		if err := json.Unmarshal(event.Payload, &result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	return results
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRunnerRejectsSecondTurn(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepare(t, "", true))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{Prompt: "one"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		handle.Cancel()
		waitDone(t, handle.Done)
	}()
	if _, err := timeline.Start(runner, session, LaunchSpec{Prompt: "two"}); err != ErrBusy {
		t.Fatalf("second Start = %v, want ErrBusy", err)
	}
	if !timeline.Running(session.ID()) {
		t.Fatal("Running() is false during a turn")
	}
}

func TestRunnerCancelClosesDone(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepare(t, "", true))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{Prompt: "long"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give the process a moment to start so cancel reaches a live pid.
	time.Sleep(100 * time.Millisecond)
	if !timeline.RequestCancel(session.ID()) {
		t.Fatal("RequestCancel found no turn")
	}
	waitDone(t, handle.Done)
	if timeline.Running(session.ID()) {
		t.Fatal("turn still marked running after cancel")
	}
	// A cancelled turn is not reported as a crash.
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == EventError {
			t.Fatalf("cancel stored an error event: %s", event.Payload)
		}
	}
	// The stopped process wrote no result; the turn is still closed by one.
	results := resultPayloads(t, session)
	if len(results) != 1 || !results[0].Cancelled || results[0].IsError {
		t.Fatalf("results after cancel = %+v", results)
	}
}

func TestRunnerClosesTurnWithoutResult(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewTimeline().Start(newTestRunner(t, helperPrepare(t, "", false)), session, LaunchSpec{Prompt: "p"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)
	results := resultPayloads(t, session)
	if len(results) != 1 || !results[0].IsError || results[0].Cancelled {
		t.Fatalf("results = %+v", results)
	}
}

func TestRunnerLaunchFailureIsStored(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		return nil, nil, context.DeadlineExceeded
	})
	timeline := NewTimeline()
	if _, err := timeline.Start(runner, session, LaunchSpec{Prompt: "p"}); err == nil {
		t.Fatal("Start returned no error from the launcher")
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var sawError, sawResult bool
	for _, event := range events {
		switch event.Type {
		case EventError:
			sawError = true
		case EventResult:
			sawResult = true
		}
	}
	if !sawError || !sawResult {
		t.Fatalf("events after a launch failure = %+v", events)
	}
	if timeline.Running(session.ID()) {
		t.Fatal("a failed launch left the turn running")
	}
}

func TestRunnerConcurrencyLimit(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Create(Meta{Title: "a", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(Meta{Title: "b", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(1)
	runner.Prepare = helperPrepare(t, "", true)
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, first, LaunchSpec{Prompt: "one"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		handle.Cancel()
		waitDone(t, handle.Done)
	}()

	started := time.Now()
	queued, err := timeline.Start(runner, second, LaunchSpec{Prompt: "two"})
	if err != nil {
		t.Fatalf("Start queued: %v", err)
	}
	// Waiting for a slot happens in the background: the request that started
	// the turn must not hang until another conversation finishes.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Start blocked for %s while queued", elapsed)
	}
	if !timeline.Running(second.ID()) {
		t.Fatal("a queued turn is not reported as running")
	}
	// The second turn must wait for the single slot and report that it queued.
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, _ := second.Events(0)
		seen := false
		for _, event := range events {
			if event.Type == EventStatus && strings.Contains(string(event.Payload), "queued") {
				seen = true
			}
		}
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a turn waiting for a slot did not report queued")
		}
		time.Sleep(20 * time.Millisecond)
	}
	queued.Cancel()
	waitDone(t, queued.Done)
	results := resultPayloads(t, second)
	if len(results) != 1 || !results[0].Cancelled {
		t.Fatalf("results after cancelling a queued turn = %+v", results)
	}
}

func TestDefaultLauncherDefaultProviderUsesPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := t.TempDir()
	// A fake claude on PATH records that it was invoked with the expected args.
	script := filepath.Join(bin, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Skipf("cannot write a shell script here: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd, cleanup, err := DefaultLauncher(context.Background(), LaunchSpec{Provider: "default", Prompt: "p"})
	if err != nil {
		t.Fatalf("DefaultLauncher: %v", err)
	}
	defer cleanup()
	if cmd == nil {
		t.Fatal("DefaultLauncher returned no command")
	}
	if cmd.Path != script {
		t.Fatalf("cmd.Path = %q, want %q", cmd.Path, script)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--permission-mode bypassPermissions") {
		t.Fatalf("args = %v", cmd.Args)
	}
}

func TestDefaultLauncherUnknownProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, _, err := DefaultLauncher(context.Background(), LaunchSpec{Provider: "nope", Prompt: "p"}); err == nil {
		t.Fatal("DefaultLauncher accepted an unknown provider")
	}
}

func TestPermissionRoundTrip(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	var seenArgs []string
	runner := newTestRunner(t, func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		seenArgs = buildArgs(spec)
		cmd := exec.Command(executable, "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "CCL_WEBUI_HELPER=1", "CCL_WEBUI_HELPER_PERMISSION=1")
		return cmd, func() {}, nil
	})
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{Prompt: "pick a db", PermissionMode: "default"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(strings.Join(seenArgs, " "), "--permission-prompt-tool stdio") {
		t.Fatalf("args = %v", seenArgs)
	}
	// Wait for the request to reach the history.
	var request PermissionPayload
	deadline := time.Now().Add(10 * time.Second)
	for request.RequestID == "" {
		events, _ := session.Events(0)
		for _, event := range events {
			if event.Type == EventPermission {
				_ = json.Unmarshal(event.Payload, &request)
				if strings.Contains(string(event.Payload), "hush") {
					t.Fatalf("a credential reached the history: %s", event.Payload)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no permission request was stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if request.ToolName != "AskUserQuestion" || !request.CanRemember {
		t.Fatalf("request = %+v", request)
	}
	// A wrong id is refused; the real one is answered once.
	if err := timeline.Respond(session, PermissionDecision{RequestID: "nope", Behavior: "allow"}); err == nil {
		t.Fatal("answered an unknown request")
	}
	decision := PermissionDecision{RequestID: "req-1", Behavior: "allow", Answers: map[string]string{"Which db?": "sqlite"}, Remember: true, Mode: "acceptEdits"}
	if err := timeline.Respond(session, decision); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if err := timeline.Respond(session, decision); err == nil {
		t.Fatal("answered the same request twice")
	}
	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var resultText string
	for _, event := range events {
		if event.Type == EventToken {
			resultText += string(event.Payload)
		}
	}
	// The helper echoes what arrived on its stdin.
	for _, want := range []string{`\"request_id\":\"req-1\"`, `\"answers\":{\"Which db?\":\"sqlite\"}`, `\"token\":\"hush\"`, `\"destination\":\"session\"`, `\"setMode\"`, `\"request_id\":\"req-2\"`, `\"subtype\":\"error\"`} {
		if !strings.Contains(resultText, want) {
			t.Fatalf("stdin lacked %s:\n%s", want, resultText)
		}
	}
	if strings.Contains(resultText, "localSettings") {
		t.Fatal("a suggested rule kept its settings-file destination")
	}
	if meta := session.Meta(); meta.PermissionMode != "acceptEdits" {
		t.Fatalf("mode after approval = %q", meta.PermissionMode)
	}
	var resolved bool
	for _, event := range events {
		if event.Type == EventPermissionResolved && strings.Contains(string(event.Payload), `"behavior":"allow"`) {
			resolved = true
		}
	}
	if !resolved {
		t.Fatal("the answer was not recorded")
	}
}

func TestPermissionDenyMessage(t *testing.T) {
	line, err := permissionResponse(pendingPermission{input: json.RawMessage(`{"command":"rm -rf x"}`)}, PermissionDecision{RequestID: "r", Behavior: "deny"})
	if err != nil || !strings.Contains(string(line), `"behavior":"deny","message":"The user declined this in ccl web."`) {
		t.Fatalf("deny = %s (%v)", line, err)
	}
	if _, err := permissionResponse(pendingPermission{}, PermissionDecision{RequestID: "r", Behavior: "maybe"}); err == nil {
		t.Fatal("accepted an unknown behavior")
	}
}
