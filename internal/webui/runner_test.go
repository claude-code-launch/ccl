package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
	acp "github.com/coder/acp-go-sdk"
)

func TestBuildArgs(t *testing.T) {
	args := buildArgs(LaunchSpec{Prompt: "hello world"})
	want := []string{"agent", "--no-leader", "--always-approve", "stdio"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}
	// The prompt never travels in argv: it is sent over ACP session/prompt.
	blocks, err := promptBlocks(LaunchSpec{Prompt: "hello world"})
	if err != nil || len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "hello world" {
		t.Fatalf("blocks = %+v (%v)", blocks, err)
	}
	// A mode that asks omits --always-approve so Grok Build sends RequestPermission over stdio.
	asking := strings.Join(buildArgs(LaunchSpec{Prompt: "p", PermissionMode: "default"}), " ")
	if asking != "agent --no-leader stdio" {
		t.Fatalf("asking args = %q", asking)
	}
}

func TestBuildArgsAddsModelAndEffortStrippingOneM(t *testing.T) {
	args := buildArgs(LaunchSpec{Prompt: "p", Model: "grok-4-heavy[1m]", Effort: "xhigh"})
	joined := strings.Join(args, "\x00")
	for _, fragment := range []string{"\x00-m\x00grok-4-heavy\x00", "\x00--reasoning-effort\x00xhigh\x00"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("missing %q in %v", fragment, args)
		}
	}
	// Empty knobs are omitted rather than sent blank.
	plain := strings.Join(buildArgs(LaunchSpec{Prompt: "p"}), "\x00")
	if strings.Contains(plain, "-m\x00") || strings.Contains(plain, "--reasoning-effort") {
		t.Fatalf("empty spec added flags: %v", plain)
	}
	// Codex's "ultra" tier is normalized to Grok Build's "max", and invalid tiers are omitted.
	ultra := strings.Join(buildArgs(LaunchSpec{Prompt: "p", Effort: "ultra"}), "\x00")
	if !strings.Contains(ultra, "\x00--reasoning-effort\x00max\x00") {
		t.Fatalf("ultra effort not normalized to max: %v", ultra)
	}
	invalid := strings.Join(buildArgs(LaunchSpec{Prompt: "p", Effort: "invalid"}), "\x00")
	if strings.Contains(invalid, "--reasoning-effort") {
		t.Fatalf("invalid effort should be omitted: %v", invalid)
	}
}

type fakeACPAgent struct {
	conn       *acp.AgentSideConnection
	cancelOnce sync.Once
	cancelled  chan struct{}

	mu          sync.Mutex
	sessionMeta map[string]any
	modes       []string
	configCalls []string
}

func newFakeACPAgent() *fakeACPAgent {
	return &fakeACPAgent{cancelled: make(chan struct{})}
}

func (a *fakeACPAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
		},
		AuthMethods: []acp.AuthMethod{
			{Agent: &acp.AuthMethodAgent{Id: "xai.api_key", Name: "API Key"}},
		},
	}, nil
}

func (a *fakeACPAgent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *fakeACPAgent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *fakeACPAgent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	a.sessionMeta = params.Meta
	a.mu.Unlock()
	sid := os.Getenv("CCL_WEBUI_HELPER_SESSION_ID")
	if sid == "" {
		sid = "grok-1"
	}
	if cmds := os.Getenv("CCL_WEBUI_HELPER_COMMANDS"); cmds != "" {
		var list []acp.AvailableCommand
		for _, name := range strings.Split(cmds, ",") {
			list = append(list, acp.AvailableCommand{Name: name, Description: name})
		}
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: acp.SessionId(sid),
			Update: acp.SessionUpdate{
				AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
					AvailableCommands: list,
				},
			},
		})
	}
	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

func (a *fakeACPAgent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	a.mu.Lock()
	a.sessionMeta = params.Meta
	a.mu.Unlock()
	// Replay a historical chunk during LoadSession; runner must suppress it.
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update:    acp.UpdateAgentMessageText("REPLAYED_HISTORY_SHOULD_BE_SUPPRESSED"),
	})
	if os.Getenv("CCL_WEBUI_HELPER_CONFIG_OPTIONS") == "1" {
		modelOpts := acp.SessionConfigSelectOptionsUngrouped{
			{Value: "claude-opus-4-6", Name: "claude-opus-4-6"},
			{Value: "gpt-5.4", Name: "gpt-5.4"},
		}
		effortOpts := acp.SessionConfigSelectOptionsUngrouped{
			{Value: "low", Name: "Low"},
			{Value: "medium", Name: "Medium"},
			{Value: "high", Name: "High"},
			{Value: "max", Name: "Max"},
		}
		return acp.LoadSessionResponse{
			ConfigOptions: []acp.SessionConfigOption{
				{Select: &acp.SessionConfigOptionSelect{Id: "model", Name: "Model", Type: "select", CurrentValue: "claude-opus-4-6", Options: acp.SessionConfigSelectOptions{Ungrouped: &modelOpts}}},
				{Select: &acp.SessionConfigOptionSelect{Id: "reasoning_effort", Name: "Reasoning Effort", Type: "select", CurrentValue: "low", Options: acp.SessionConfigSelectOptions{Ungrouped: &effortOpts}}},
			},
		}, nil
	}
	return acp.LoadSessionResponse{}, nil
}

func (a *fakeACPAgent) Cancel(_ context.Context, _ acp.CancelNotification) error {
	a.cancelOnce.Do(func() { close(a.cancelled) })
	return nil
}

func (a *fakeACPAgent) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (a *fakeACPAgent) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}

func (a *fakeACPAgent) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (a *fakeACPAgent) SetSessionConfigOption(_ context.Context, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if req.ValueId != nil {
		a.mu.Lock()
		a.configCalls = append(a.configCalls, string(req.ValueId.ConfigId)+"="+string(req.ValueId.Value))
		a.mu.Unlock()
		if os.Getenv("CCL_WEBUI_HELPER_CONFIG_OPTIONS") == "1" && req.ValueId.ConfigId == "model" && req.ValueId.Value == "gpt-5.4" {
			modelOpts := acp.SessionConfigSelectOptionsUngrouped{
				{Value: "claude-opus-4-6", Name: "claude-opus-4-6"},
				{Value: "gpt-5.4", Name: "gpt-5.4"},
			}
			gptEfforts := acp.SessionConfigSelectOptionsUngrouped{
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
				{Value: "xhigh", Name: "X-High"},
			}
			return acp.SetSessionConfigOptionResponse{
				ConfigOptions: []acp.SessionConfigOption{
					{Select: &acp.SessionConfigOptionSelect{Id: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.4", Options: acp.SessionConfigSelectOptions{Ungrouped: &modelOpts}}},
					{Select: &acp.SessionConfigOptionSelect{Id: "reasoning_effort", Name: "Reasoning Effort", Type: "select", CurrentValue: "medium", Options: acp.SessionConfigSelectOptions{Ungrouped: &gptEfforts}}},
				},
			}, nil
		}
	}
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (a *fakeACPAgent) SetSessionMode(_ context.Context, req acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	a.mu.Lock()
	a.modes = append(a.modes, string(req.ModeId))
	a.mu.Unlock()
	return acp.SetSessionModeResponse{}, nil
}

func (a *fakeACPAgent) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "_x.ai/mcp/list" && os.Getenv("CCL_WEBUI_HELPER_MCP") == "1" {
		return map[string]any{
			"servers": []map[string]any{
				{"name": "github", "session": map[string]any{"enabled": true, "status": "connected"}},
				{"name": "linear", "session": map[string]any{"enabled": false, "status": "connected"}},
			},
		}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func (a *fakeACPAgent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	if os.Getenv("CCL_WEBUI_HELPER_ACCEPT_EDITS") == "1" {
		editKind := acp.ToolKindEdit
		editTitle := "search_replace"
		editResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tu-edit",
				Kind:       &editKind,
				Title:      &editTitle,
				RawInput:   map[string]any{"file_path": "/w/a.go", "old_string": "a", "new_string": "b"},
			},
			Options: []acp.PermissionOption{
				{OptionId: "edit-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "edit-deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		execKind := acp.ToolKindExecute
		execTitle := "run_terminal_command"
		execResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tu-exec",
				Kind:       &execKind,
				Title:      &execTitle,
				RawInput:   map[string]any{"command": "rm -rf /tmp/x"},
			},
			Options: []acp.PermissionOption{
				{OptionId: "exec-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "exec-deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		a.mu.Lock()
		modes := strings.Join(a.modes, ",")
		autoMode, _ := a.sessionMeta["autoMode"].(bool)
		a.mu.Unlock()
		editOpt := ""
		if editResp.Outcome.Selected != nil {
			editOpt = string(editResp.Outcome.Selected.OptionId)
		}
		execOpt := ""
		if execResp.Outcome.Selected != nil {
			execOpt = string(execResp.Outcome.Selected.OptionId)
		}
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText("auto=" + strconv.FormatBool(autoMode) + " modes=" + modes + " edit=" + editOpt + " exec=" + execOpt),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	if os.Getenv("CCL_WEBUI_HELPER_ASK_PLAN") == "1" {
		askTitle := "ask_user_question"
		askResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tu-ask",
				Title:      &askTitle,
				RawInput: map[string]any{
					"questions": []map[string]any{
						{"question": "Target env?", "options": []map[string]any{{"label": "staging"}}},
					},
				},
			},
			Options: []acp.PermissionOption{
				{OptionId: "ask-allow", Name: "Submit", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "ask-deny", Name: "Cancel", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		switchKind := acp.ToolKindSwitchMode
		planTitle := "exit_plan_mode"
		planResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tu-plan",
				Kind:       &switchKind,
				Title:      &planTitle,
				RawInput:   map[string]any{"planContent": "## Step 1\nShip it"},
			},
			Options: []acp.PermissionOption{
				{OptionId: "plan-allow", Name: "Approve", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "plan-deny", Name: "Keep planning", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		// Give async SetSessionMode a moment to arrive before reading a.modes.
		time.Sleep(50 * time.Millisecond)
		a.mu.Lock()
		modes := strings.Join(a.modes, ",")
		a.mu.Unlock()
		metaJSON, _ := json.Marshal(map[string]any{
			"askMeta":  askResp.Outcome.Selected.Meta,
			"planMeta": planResp.Outcome.Selected.Meta,
			"modes":    modes,
		})
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText(string(metaJSON)),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if os.Getenv("CCL_WEBUI_HELPER_PERMISSION") == "1" {
		kind := acp.ToolKindExecute
		title := "Run db migration"
		resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tu-1",
				Kind:       &kind,
				Title:      &title,
				RawInput:   map[string]any{"command": "migrate up", "token": "hush"},
				Locations:  []acp.ToolCallLocation{{Path: "/w/db.sql"}},
			},
			Options: []acp.PermissionOption{
				{OptionId: "opt-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "opt-always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
				{OptionId: "opt-deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		chosen := "cancelled"
		if resp.Outcome.Selected != nil {
			chosen = string(resp.Outcome.Selected.OptionId)
		}
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText("selected:" + chosen),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	if os.Getenv("CCL_WEBUI_HELPER_SLEEP") != "" {
		select {
		case <-a.cancelled:
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		case <-ctx.Done():
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		case <-time.After(30 * time.Second):
		}
	}

	if os.Getenv("CCL_WEBUI_HELPER_SUBAGENTS") == "1" {
		otherKind := acp.ToolKindOther
		completed := acp.ToolCallStatusCompleted
		desc := "Check weather"
		xaiMeta := map[string]any{
			"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"},
		}
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "call_sub_1",
					Title:      "spawn_subagent",
					RawInput:   map[string]any{"prompt": "Forecast", "description": desc},
					Meta:       xaiMeta,
					Status:     acp.ToolCallStatusPending,
				},
			},
		})
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update: acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "call_sub_1",
					Kind:       &otherKind,
					Title:      &desc,
					RawInput:   map[string]any{"variant": "Task", "prompt": "Forecast", "description": desc},
					Meta:       xaiMeta,
				},
			},
		})
		_ = a.conn.NotifyExtension(ctx, "_x.ai/session/update", map[string]any{
			"sessionId": string(params.SessionId),
			"update": map[string]any{
				"sessionUpdate": "subagent_spawned",
				"subagent_id":   "sub_run_1",
				"subagent_type": "general-purpose",
				"description":   desc,
			},
		})
		_ = a.conn.NotifyExtension(ctx, "_x.ai/session/update", map[string]any{
			"sessionId": string(params.SessionId),
			"update": map[string]any{
				"sessionUpdate": "subagent_finished",
				"subagent_id":   "sub_run_1",
				"status":        "completed",
				"tool_calls":    4,
				"duration_ms":   1500,
				"tokens_used":   900,
			},
		})
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update: acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "call_sub_1",
					Status:     &completed,
					Content:    []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Sunny tomorrow"))},
					RawOutput: map[string]any{
						"type":          "SubagentCompleted",
						"subagent_id":   "sub_run_1",
						"subagent_type": "general-purpose",
						"tool_calls":    4,
						"duration_ms":   1500,
						"output":        "Sunny tomorrow",
					},
				},
			},
		})
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText("done"),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	if costText := os.Getenv("CCL_WEBUI_HELPER_COST"); costText != "" {
		if cost, err := strconv.ParseFloat(costText, 64); err == nil {
			_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: params.SessionId,
				Update: acp.SessionUpdate{
					UsageUpdate: &acp.SessionUsageUpdate{
						Used: 512,
						Size: 200000,
						Cost: &acp.Cost{Amount: cost, Currency: "USD"},
					},
				},
			})
		}
	}

	if text := os.Getenv("CCL_WEBUI_HELPER_TEXT"); text != "" {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText(text),
		})
	}
	if os.Getenv("CCL_WEBUI_HELPER_CONFIG_OPTIONS") == "1" {
		a.mu.Lock()
		calls := strings.Join(a.configCalls, ",")
		a.mu.Unlock()
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    acp.UpdateAgentMessageText("config_calls:" + calls),
		})
	}

	inTok, _ := strconv.Atoi(os.Getenv("CCL_WEBUI_HELPER_USAGE_IN"))
	outTok, _ := strconv.Atoi(os.Getenv("CCL_WEBUI_HELPER_USAGE_OUT"))
	var usage *acp.Usage
	if inTok > 0 || outTok > 0 {
		usage = &acp.Usage{
			InputTokens:  inTok,
			OutputTokens: outTok,
			TotalTokens:  inTok + outTok,
		}
	}
	return acp.PromptResponse{
		StopReason: acp.StopReasonEndTurn,
		Usage:      usage,
	}, nil
}

// TestHelperProcess is not a test: the runner tests re-execute this binary so
// they get a real ACP child process without depending on an external grok CLI.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CCL_WEBUI_HELPER") != "1" {
		t.Skip("helper process")
	}
	if os.Getenv("CCL_WEBUI_HELPER_CRASH") == "1" {
		_, _ = os.Stderr.WriteString("fatal helper crash\n")
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.DiscardHandler))
	agent := newFakeACPAgent()
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.conn = conn
	<-conn.Done()
	if os.Getenv("CCL_WEBUI_HELPER_LINGER") == "1" {
		time.Sleep(500 * time.Millisecond)
	}
	os.Exit(0)
}

func helperPrepare(t *testing.T, text string, sleep bool) LaunchFunc {
	t.Helper()
	return helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_TEXT":      text,
		"CCL_WEBUI_HELPER_USAGE_IN":  "2",
		"CCL_WEBUI_HELPER_USAGE_OUT": "3",
		"CCL_WEBUI_HELPER_COST":      "0.01",
	}, sleep)
}

func helperPrepareEnv(t *testing.T, extra map[string]string, sleep bool) LaunchFunc {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		cmd := exec.Command(executable, "-test.run=TestHelperProcess")
		env := append(os.Environ(), "CCL_WEBUI_HELPER=1")
		for k, v := range extra {
			env = append(env, k+"="+v)
		}
		if sleep {
			env = append(env, "CCL_WEBUI_HELPER_SLEEP=1")
		}
		cmd.Env = env
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
	runner := newTestRunner(t, helperPrepare(t, "hi", false))
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
	if meta.AgentSessionID != "grok-1" {
		t.Fatalf("AgentSessionID = %q, want grok-1", meta.AgentSessionID)
	}
	if meta.Usage.InputTokens != 2 || meta.Usage.OutputTokens != 3 || meta.Usage.CostUSD != 0.01 {
		t.Fatalf("usage = %+v", meta.Usage)
	}
}

func TestRunnerResumesSessionAndSuppressesReplayChunks(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Provider: "default", Workspace: t.TempDir(), AgentSessionID: "grok-existing"})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepare(t, "fresh reply", false))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Provider:  "default",
		Workspace: session.Meta().Workspace,
		Resume:    "grok-existing",
		Prompt:    "next turn",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)

	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	for _, ev := range events {
		if ev.Type == EventToken {
			var tok TokenPayload
			_ = json.Unmarshal(ev.Payload, &tok)
			streamed.WriteString(tok.Text)
		}
	}
	if streamed.String() != "fresh reply" {
		t.Fatalf("streamed text = %q, want %q (replayed history must be suppressed)", streamed.String(), "fresh reply")
	}
}

func TestRunnerTurnCostIsDifferenceOfRunningTotal(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewTimeline()
	for _, cost := range []string{"0.015", "0.04"} {
		prepare := helperPrepareEnv(t, map[string]string{
			"CCL_WEBUI_HELPER_USAGE_IN":  "5",
			"CCL_WEBUI_HELPER_USAGE_OUT": "1",
			"CCL_WEBUI_HELPER_COST":      cost,
		}, false)
		handle, err := timeline.Start(newTestRunner(t, prepare), session, LaunchSpec{Prompt: "p"})
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
	if !nearly(meta.Usage.CostUSD, 0.04) || meta.Usage.InputTokens != 10 {
		t.Fatalf("conversation usage = %+v", meta.Usage)
	}
}

func TestMetaRecordTurnIgnoresZeroedAndRestartedTotals(t *testing.T) {
	meta := Meta{}
	meta.recordTurn(Usage{CostUSD: 0.02})
	if turn := meta.recordTurn(Usage{}); turn.CostUSD != 0 || meta.SessionCostUSD != 0.02 {
		t.Fatalf("zeroed result: turn=%+v meta=%+v", turn, meta)
	}
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
	time.Sleep(100 * time.Millisecond)
	if !timeline.RequestCancel(session.ID()) {
		t.Fatal("RequestCancel found no turn")
	}
	waitDone(t, handle.Done)
	if timeline.Running(session.ID()) {
		t.Fatal("turn still marked running after cancel")
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == EventError {
			t.Fatalf("cancel stored an error event: %s", event.Payload)
		}
	}
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
	crashPrepare := helperPrepareEnv(t, map[string]string{"CCL_WEBUI_HELPER_CRASH": "1"}, false)
	handle, err := NewTimeline().Start(newTestRunner(t, crashPrepare), session, LaunchSpec{Prompt: "p"})
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
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Start blocked for %s while queued", elapsed)
	}
	if !timeline.Running(second.ID()) {
		t.Fatal("a queued turn is not reported as running")
	}
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

func TestDefaultLauncherDefaultProviderUsesGrok(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_SANDBOX", "off")
	bin := t.TempDir()
	script := filepath.Join(bin, "grok")
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
	if got := strings.Join(cmd.Args[1:], " "); got != "agent --no-leader --always-approve stdio" {
		t.Fatalf("args = %v", cmd.Args)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "GROK_SANDBOX=off") || !strings.Contains(env, "GROK_SANDBOX=workspace") || !strings.Contains(env, "GROK_DISABLE_AUTOUPDATER=1") || strings.Contains(env, "GROK_CONFIG=") {
		t.Fatalf("unexpected env = %s", env)
	}

	// Session worktrees with a synthesized .grok/sandbox.toml profile must select GROK_SANDBOX=ccl_worktree.
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /tmp/repo/.git/worktrees/w1\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(wt, ".grok"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".grok", "sandbox.toml"), []byte("[profiles.ccl_worktree]\nextends = \"workspace\"\n"), 0o600)
	wtCmd, wtClean, err := DefaultLauncher(context.Background(), LaunchSpec{Provider: "default", Workspace: wt, Prompt: "p"})
	if err != nil {
		t.Fatalf("DefaultLauncher(worktree): %v", err)
	}
	defer wtClean()
	if !strings.Contains(strings.Join(wtCmd.Env, "\n"), "GROK_SANDBOX=ccl_worktree") {
		t.Fatalf("expected GROK_SANDBOX=ccl_worktree in %v", wtCmd.Env)
	}

	// LegacyClaudeDefault must refuse launching silently on default until cleared.
	if err := config.Update(func(cfg *provider.Config) error {
		cfg.LegacyClaudeDefault = "old-claude-profile"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DefaultLauncher(context.Background(), LaunchSpec{Provider: "default", Prompt: "p"}); err == nil {
		t.Fatal("DefaultLauncher accepted retired LegacyClaudeDefault")
	}
}

func TestDefaultLauncherUnknownProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := t.TempDir()
	script := filepath.Join(bin, "grok")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	var seenArgs []string
	basePrepare := helperPrepareEnv(t, map[string]string{"CCL_WEBUI_HELPER_PERMISSION": "1"}, false)
	runner := newTestRunner(t, func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		seenArgs = buildArgs(spec)
		return basePrepare(ctx, spec)
	})
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{Prompt: "run migration", PermissionMode: "default"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if strings.Contains(strings.Join(seenArgs, " "), "--always-approve") {
		t.Fatalf("default permission mode passed --always-approve: %v", seenArgs)
	}

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
	if request.ToolName != "Bash" || !request.CanRemember || request.BlockedPath != "/w/db.sql" {
		t.Fatalf("request = %+v", request)
	}
	// Unknown request id is refused; real one is answered once.
	if err := timeline.Respond(session, PermissionDecision{RequestID: "nope", Behavior: "allow"}); !errors.Is(err, errUnknownRequest) {
		t.Fatalf("unknown request = %v, want errUnknownRequest", err)
	}
	decision := PermissionDecision{RequestID: request.RequestID, Behavior: "allow", Remember: true, Mode: "acceptEdits"}
	if err := timeline.Respond(session, decision); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if err := timeline.Respond(session, decision); err == nil {
		t.Fatal("answered the same request twice")
	}
	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var resultText string
	var resolved bool
	for _, event := range events {
		if event.Type == EventToken {
			var tok TokenPayload
			_ = json.Unmarshal(event.Payload, &tok)
			resultText += tok.Text
		}
		if event.Type == EventPermissionResolved && strings.Contains(string(event.Payload), `"behavior":"allow"`) {
			resolved = true
		}
	}
	if resultText != "selected:opt-always" {
		t.Fatalf("agent received %q, want selected:opt-always", resultText)
	}
	if meta := session.Meta(); meta.PermissionMode != "acceptEdits" {
		t.Fatalf("mode after approval = %q", meta.PermissionMode)
	}
	if !resolved {
		t.Fatal("the answer was not recorded")
	}
}

func TestPermissionOptionSelection(t *testing.T) {
	pending := pendingPermission{
		allowOnce:   "once",
		allowAlways: "always",
		rejectOnce:  "reject",
	}
	if opt, ok := pending.optionFor(PermissionDecision{Behavior: "allow"}); !ok || opt != "once" {
		t.Fatalf("allow once = %q, %v", opt, ok)
	}
	if opt, ok := pending.optionFor(PermissionDecision{Behavior: "allow", Remember: true}); !ok || opt != "always" {
		t.Fatalf("allow always = %q, %v", opt, ok)
	}
	if opt, ok := pending.optionFor(PermissionDecision{Behavior: "deny"}); !ok || opt != "reject" {
		t.Fatalf("deny = %q, %v", opt, ok)
	}
	if _, ok := pending.optionFor(PermissionDecision{Behavior: "maybe"}); ok {
		t.Fatal("accepted an unknown behavior")
	}
}

func TestRunnerMarksSubscriptionResults(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewTimeline().Start(newTestRunner(t, helperPrepare(t, "ok", false)), session,
		LaunchSpec{Provider: "sub", Subscription: true, Workspace: session.Meta().Workspace, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, handle.Done)
	events, _ := session.Events(0)
	last := events[len(events)-1]
	if last.Type != EventResult || !strings.Contains(string(last.Payload), `"subscription":true`) {
		t.Fatalf("last event = %s %s", last.Type, last.Payload)
	}
}

func TestAcceptEditsAutoApprovesEditOnlyAndMCPInit(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_ACCEPT_EDITS": "1",
		"CCL_WEBUI_HELPER_MCP":          "1",
		"CCL_WEBUI_HELPER_COMMANDS":     "review,commit",
	}, false))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Workspace:      session.Meta().Workspace,
		Prompt:         "edit and run",
		PermissionMode: "acceptEdits",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Only the Bash execute tool should prompt; the Edit tool must have been auto-approved.
	var request PermissionPayload
	deadline := time.Now().Add(10 * time.Second)
	for request.RequestID == "" {
		events, _ := session.Events(0)
		for _, event := range events {
			if event.Type == EventPermission {
				_ = json.Unmarshal(event.Payload, &request)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Bash permission request")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if request.ToolName != "Bash" {
		t.Fatalf("expected only Bash permission prompt, got %+v", request)
	}
	if err := timeline.Respond(session, PermissionDecision{RequestID: request.RequestID, Behavior: "allow"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var out string
	var permCount int
	var lastInit InitPayload
	for _, ev := range events {
		switch ev.Type {
		case EventPermission:
			permCount++
		case EventInit:
			_ = json.Unmarshal(ev.Payload, &lastInit)
		case EventToken:
			var tok TokenPayload
			_ = json.Unmarshal(ev.Payload, &tok)
			out += tok.Text
		}
	}
	if permCount != 1 {
		t.Fatalf("permission prompt count = %d, want 1", permCount)
	}
	if out != "auto=true modes=auto edit=edit-once exec=exec-once" {
		t.Fatalf("unexpected turn summary %q", out)
	}
	if len(lastInit.MCPServers) != 2 || lastInit.MCPServers[0].Name != "github" || lastInit.MCPServers[0].Status != "connected" ||
		lastInit.MCPServers[1].Name != "linear" || lastInit.MCPServers[1].Status != "disabled" {
		t.Fatalf("init MCPServers = %+v", lastInit.MCPServers)
	}
}

func TestAskUserQuestionAndExitPlanModeRoundTrip(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_ASK_PLAN": "1",
	}, false))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Workspace:      session.Meta().Workspace,
		Prompt:         "plan deployment",
		PermissionMode: "plan",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitPerm := func(seenID string) PermissionPayload {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			events, _ := session.Events(0)
			for _, ev := range events {
				if ev.Type != EventPermission {
					continue
				}
				var p PermissionPayload
				_ = json.Unmarshal(ev.Payload, &p)
				if p.RequestID != "" && p.RequestID != seenID {
					return p
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for permission request")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	askReq := waitPerm("")
	if askReq.ToolName != "AskUserQuestion" {
		t.Fatalf("askReq.ToolName = %q, want AskUserQuestion", askReq.ToolName)
	}
	if err := timeline.Respond(session, PermissionDecision{
		RequestID: askReq.RequestID,
		Behavior:  "allow",
		Answers:   map[string]string{"Target env?": "staging"},
	}); err != nil {
		t.Fatalf("Respond(ask): %v", err)
	}

	planReq := waitPerm(askReq.RequestID)
	if planReq.ToolName != "ExitPlanMode" || !strings.Contains(string(planReq.Input), `"plan":"## Step 1\nShip it"`) {
		t.Fatalf("planReq = %+v (%s)", planReq, planReq.Input)
	}
	if err := timeline.Respond(session, PermissionDecision{
		RequestID: planReq.RequestID,
		Behavior:  "allow",
		Message:   "Approved for staging",
		Mode:      "acceptEdits",
	}); err != nil {
		t.Fatalf("Respond(plan): %v", err)
	}

	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var text string
	for _, ev := range events {
		if ev.Type == EventToken {
			var tok TokenPayload
			_ = json.Unmarshal(ev.Payload, &tok)
			text += tok.Text
		}
	}
	if !strings.Contains(text, `"Target env?":"staging"`) || !strings.Contains(text, `"message":"Approved for staging"`) || !strings.Contains(text, `"modes":"plan,auto"`) {
		t.Fatalf("unexpected ask/plan round-trip output: %s", text)
	}
}

func TestRunnerLoadSessionSyncsModelAndClampsReasoningEffort(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "resume-route", Workspace: t.TempDir(), AgentSessionID: "grok-saved"})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_CONFIG_OPTIONS": "1",
	}, false))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Prompt:    "switch route on resume",
		Workspace: session.Meta().Workspace,
		Resume:    "grok-saved",
		Model:     "gpt-5.4[1m]",
		Effort:    "ultra",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var text string
	for _, ev := range events {
		if ev.Type == EventToken {
			var tok TokenPayload
			_ = json.Unmarshal(ev.Payload, &tok)
			text += tok.Text
		}
	}
	// LoadSession started on claude-opus-4-6/low; switching to gpt-5.4 with ultra ("max")
	// updates model=gpt-5.4, reads gpt-5.4's updated effort menu (low..xhigh), and clamps max -> xhigh.
	if !strings.Contains(text, "config_calls:model=gpt-5.4,reasoning_effort=xhigh") {
		t.Fatalf("unexpected config sync calls: %q", text)
	}
}

func TestRunnerLoadSessionResetsEmptyModelAndEffortToRouteDefaults(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "resume-reset-default", Workspace: t.TempDir(), AgentSessionID: "grok-saved"})
	if err != nil {
		t.Fatal(err)
	}
	basePrepare := helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_CONFIG_OPTIONS": "1",
	}, false)
	runner := newTestRunner(t, func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		if spec.routeDefaults != nil {
			*spec.routeDefaults = acpRouteDefaults{
				model:  "gpt-5.4",
				effort: "high",
			}
		}
		return basePrepare(ctx, spec)
	})
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Prompt:    "reset to provider defaults on resume",
		Workspace: session.Meta().Workspace,
		Resume:    "grok-saved",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)

	events, _ := session.Events(0)
	var (
		text      string
		initModel string
	)
	for _, ev := range events {
		switch ev.Type {
		case EventToken:
			var tok TokenPayload
			_ = json.Unmarshal(ev.Payload, &tok)
			text += tok.Text
		case EventInit:
			var init InitPayload
			_ = json.Unmarshal(ev.Payload, &init)
			initModel = init.Model
		}
	}
	if initModel != "gpt-5.4" {
		t.Fatalf("EventInit.Model = %q, want %q", initModel, "gpt-5.4")
	}
	if !strings.Contains(text, "config_calls:model=gpt-5.4,reasoning_effort=high") {
		t.Fatalf("unexpected config sync calls: %q", text)
	}
}

func TestResolveManagedDefaultEffort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cachePath, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes, err := os.ReadFile(filepath.Join("..", "modelsdev", "modelsdev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, catalogBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	p := provider.Provider{
		EffortLevel: "ultra",
		Model:       "gpt-5.4",
	}
	if got := resolveManagedDefaultEffort(p, "gpt-5.4", nil); got != "xhigh" {
		t.Fatalf("resolveManagedDefaultEffort(ultra on gpt-5.4) = %q, want xhigh", got)
	}
	p.EffortLevel = ""
	if got := resolveManagedDefaultEffort(p, "claude-opus-4-6-thinking", nil); got != "medium" {
		t.Fatalf("resolveManagedDefaultEffort(claude-opus-4-6-thinking) = %q, want medium", got)
	}
	if got := resolveManagedDefaultEffort(p, "gpt-4o", nil); got != "" {
		t.Fatalf("resolveManagedDefaultEffort(gpt-4o) = %q, want empty", got)
	}
}

func TestRunnerStreamsGrokSubagentLifecycleAndExtensionUsage(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "Subagents", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, helperPrepareEnv(t, map[string]string{
		"CCL_WEBUI_HELPER_SUBAGENTS": "1",
	}, false))
	timeline := NewTimeline()
	handle, err := timeline.Start(runner, session, LaunchSpec{
		Workspace:      session.Meta().Workspace,
		Prompt:         "run subagent",
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, handle.Done)

	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var toolUses []ToolUsePayload
	var completedTasks []TaskPayload
	for _, ev := range events {
		switch ev.Type {
		case EventToolUse:
			var u ToolUsePayload
			_ = json.Unmarshal(ev.Payload, &u)
			toolUses = append(toolUses, u)
		case EventTask:
			var task TaskPayload
			_ = json.Unmarshal(ev.Payload, &task)
			if task.Status == "completed" {
				completedTasks = append(completedTasks, task)
			}
		}
	}
	if len(toolUses) == 0 {
		t.Fatal("expected tool_use events")
	}
	for _, u := range toolUses {
		if u.Name != "Task" {
			t.Fatalf("tool_use.Name = %q, want Task (%+v)", u.Name, u)
		}
	}
	if len(completedTasks) != 1 {
		t.Fatalf("completed tasks = %+v, want 1", completedTasks)
	}
	got := completedTasks[0]
	if got.ID != "call_sub_1" || got.Agent != "general-purpose" || got.Summary != "Sunny tomorrow" {
		t.Fatalf("completed task = %+v", got)
	}
	if got.Usage == nil || got.Usage.Tokens != 900 || got.Usage.ToolUses != 4 || got.Usage.DurationMs != 1500 {
		t.Fatalf("completed task usage = %+v", got.Usage)
	}
}
