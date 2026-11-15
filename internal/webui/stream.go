package webui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// Event types pushed to the browser and persisted in events.jsonl.
const (
	EventInit       = "init"
	EventToken      = "token"
	EventThinking   = "thinking"
	EventContext    = "context"
	EventToolUse    = "tool_use"
	EventToolResult = "tool_result"
	EventResult     = "result"
	EventError      = "error"
	EventQueued     = "queued"
	EventUser       = "user"
	EventStatus     = "status"
	EventTask       = "task"
)

// SummaryLimit is the rune count of a tool call's one-line summary.
const SummaryLimit = 200

// ToolOutputLineLimit bounds how many lines of a tool result reach the browser.
const ToolOutputLineLimit = 2000

// UserPayload is a message the user sent.
type UserPayload struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Mirrored marks an imported prompt, not a turn started from this page.
	Mirrored bool `json:"mirrored,omitempty"`
	// Steered marks a message sent into a running turn.
	Steered bool `json:"steered,omitempty"`
}

// InitPayload reports the ACP session a turn runs in and what it offers.
type InitPayload struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	// SlashCommands are the commands a prompt can start with.
	SlashCommands []string `json:"slashCommands,omitempty"`
	// MCPServers are the connectors loaded for the session, with their state.
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

// MCPServer is one MCP server reported for a session.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// TokenPayload is one streamed fragment of assistant text (or, as a
// thinking event, of its reasoning).
type TokenPayload struct {
	Text string `json:"text"`
	// Subagent marks text produced by a sub-agent, which the UI indents.
	Subagent bool `json:"subagent,omitempty"`
	// Parent is the Task call a sub-agent's event belongs to.
	Parent string `json:"parent,omitempty"`
}

// ContextPayload is how many tokens the latest model call read: the size of
// the conversation's context at that point.
type ContextPayload struct {
	Tokens int64 `json:"tokens"`
}

// ToolUsePayload reports a tool call the assistant started.
type ToolUsePayload struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Description is the call's own one-line explanation, which the page shows
	// in place of or alongside the raw arguments.
	Description string `json:"description,omitempty"`
	// Input is the call's arguments with credentials masked and long strings
	// cut (see toolInput): what the page needs for diffs, todo lists and plans.
	Input    json.RawMessage `json:"input,omitempty"`
	Subagent bool            `json:"subagent,omitempty"`
	Parent   string          `json:"parent,omitempty"`
}

// ToolResultPayload reports the outcome of a tool call.
type ToolResultPayload struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	OK       bool   `json:"ok"`
	Output   string `json:"output"`
	Subagent bool   `json:"subagent,omitempty"`
	Parent   string `json:"parent,omitempty"`
}

// ResultPayload closes one turn.
type ResultPayload struct {
	// Usage is the turn's token/duration report and any cumulative session cost.
	Usage Usage `json:"usage"`
	// TurnUsage is what this turn alone spent.
	TurnUsage *Usage `json:"turnUsage,omitempty"`
	IsError   bool   `json:"isError"`
	// Cancelled marks a turn the user stopped; it is not a failure.
	Cancelled bool   `json:"cancelled,omitempty"`
	Message   string `json:"message,omitempty"`
	// Subscription marks a turn on a subscription (OAuth) provider.
	Subscription bool `json:"subscription,omitempty"`
}

// TaskPayload reports the state of background work started by a tool call.
type TaskPayload struct {
	// ID is the tool call that started the task.
	ID string `json:"id"`
	// Status is running, completed, failed or stopped.
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	// Agent is the sub-agent type of an agent task.
	Agent string `json:"agent,omitempty"`
	// Summary is the task's latest progress line, or how it ended.
	Summary  string     `json:"summary,omitempty"`
	LastTool string     `json:"lastTool,omitempty"`
	Usage    *TaskUsage `json:"usage,omitempty"`
}

// TaskUsage is what a task has spent so far.
type TaskUsage struct {
	Tokens     int64 `json:"tokens"`
	ToolUses   int64 `json:"toolUses"`
	DurationMs int64 `json:"durationMs"`
}

// ErrorPayload reports a failure that ended the turn early.
type ErrorPayload struct {
	Message string `json:"message"`
}

// StatusPayload reports a non-fatal state change (queued, still running).
type StatusPayload struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// emitter receives translated ccl serve events.
type emitter func(kind string, payload any)

type acpTrackedTool struct {
	id            string
	kind          acp.ToolKind
	title         string
	canonicalName string
	subagentID    string
	rawInput      any
	rawOutput     any
	locations     []acp.ToolCallLocation
	content       []acp.ToolCallContent
	status        acp.ToolCallStatus
	meta          map[string]any
	subagent      bool
	parent        string
	emitted       bool
	taskEmitted   bool
	completed     bool
}

type acpSubagentState struct {
	id             string
	childSessionID string
	subagentType   string
	description    string
	model          string
	toolCallID     string
	status         string
	tokensUsed     int64
	toolCalls      int64
	durationMs     int64
	output         string
	finished       bool
}

// acpTranslator turns Grok Build ACP SessionUpdate notifications into ccl serve events.
type acpTranslator struct {
	tools          map[string]*acpTrackedTool
	toolOrder      []string
	subagents      map[string]*acpSubagentState
	slashCommands  []string
	sessionCostUSD float64
}

func newACPTranslator() *acpTranslator {
	return &acpTranslator{
		tools:     make(map[string]*acpTrackedTool),
		subagents: make(map[string]*acpSubagentState),
	}
}

func (t *acpTranslator) SlashCommands() []string {
	return append([]string(nil), t.slashCommands...)
}

func (t *acpTranslator) SessionCostUSD() float64 {
	return t.sessionCostUSD
}

// HandleCommands extracts slash command names from AvailableCommandsUpdate.
func (t *acpTranslator) HandleCommands(update *acp.SessionAvailableCommandsUpdate) ([]string, bool) {
	if update == nil {
		return nil, false
	}
	seen := make(map[string]bool, len(update.AvailableCommands))
	commands := make([]string, 0, len(update.AvailableCommands))
	for _, cmd := range update.AvailableCommands {
		name := strings.TrimPrefix(strings.TrimSpace(cmd.Name), "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		commands = append(commands, name)
	}
	sort.Strings(commands)
	if strings.Join(commands, "\x00") == strings.Join(t.slashCommands, "\x00") {
		return t.SlashCommands(), false
	}
	t.slashCommands = commands
	return t.SlashCommands(), true
}

func extractSubagentLineage(meta map[string]any) (subagent bool, parent string) {
	if len(meta) == 0 {
		return false, ""
	}
	for _, key := range []string{"parentToolCallId", "parent_tool_call_id", "parentToolUseId", "parent_tool_use_id", "parentId", "parent_id", "parent"} {
		if val, ok := meta[key].(string); ok && strings.TrimSpace(val) != "" {
			parent = strings.TrimSpace(val)
			break
		}
	}
	if b, ok := meta["subagent"].(bool); ok && b {
		subagent = true
	}
	for _, key := range []string{"subagentId", "subagent_id", "agentId", "agent_id", "subagentType", "subagent_type"} {
		if val, ok := meta[key].(string); ok && strings.TrimSpace(val) != "" {
			subagent = true
			break
		}
	}
	if parent != "" {
		subagent = true
	}
	return subagent, parent
}

func mergeToolMeta(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Handle translates one live ACP SessionUpdate notification.
func (t *acpTranslator) Handle(update acp.SessionUpdate, emit emitter) {
	switch {
	case update.AgentThoughtChunk != nil:
		if text := contentBlockText(update.AgentThoughtChunk.Content); text != "" {
			sub, parent := extractSubagentLineage(update.AgentThoughtChunk.Meta)
			emit(EventThinking, TokenPayload{Text: text, Subagent: sub, Parent: parent})
		}

	case update.AgentMessageChunk != nil:
		if text := contentBlockText(update.AgentMessageChunk.Content); text != "" {
			sub, parent := extractSubagentLineage(update.AgentMessageChunk.Meta)
			emit(EventToken, TokenPayload{Text: text, Subagent: sub, Parent: parent})
		}

	case update.Plan != nil:
		t.emitPlan(update.Plan, emit)

	case update.ToolCall != nil:
		call := update.ToolCall
		id := string(call.ToolCallId)
		if id == "" {
			return
		}
		tracked := t.ensureTrackedTool(id)
		tracked.kind = call.Kind
		tracked.title = call.Title
		tracked.rawInput = call.RawInput
		tracked.rawOutput = call.RawOutput
		tracked.locations = call.Locations
		tracked.content = call.Content
		tracked.status = call.Status
		tracked.meta = mergeToolMeta(tracked.meta, call.Meta)
		if sub, parent := extractSubagentLineage(tracked.meta); sub {
			tracked.subagent = true
			if parent != "" {
				tracked.parent = parent
			}
		}
		t.emitToolUse(tracked, emit)
		t.maybeEmitToolResult(tracked, emit)

	case update.ToolCallUpdate != nil:
		delta := update.ToolCallUpdate
		id := string(delta.ToolCallId)
		if id == "" {
			return
		}
		tracked := t.ensureTrackedTool(id)
		needUse := !tracked.emitted
		if delta.Kind != nil && *delta.Kind != tracked.kind {
			tracked.kind = *delta.Kind
			needUse = true
		}
		if delta.Title != nil && *delta.Title != tracked.title {
			tracked.title = *delta.Title
			needUse = true
		}
		if delta.RawInput != nil {
			tracked.rawInput = delta.RawInput
			needUse = true
		}
		if delta.RawOutput != nil {
			tracked.rawOutput = delta.RawOutput
		}
		if delta.Locations != nil {
			tracked.locations = delta.Locations
			needUse = true
		}
		if delta.Content != nil {
			tracked.content = delta.Content
			if hasDiffContent(delta.Content) {
				needUse = true
			}
		}
		if delta.Status != nil {
			tracked.status = *delta.Status
		}
		if len(delta.Meta) > 0 {
			tracked.meta = mergeToolMeta(tracked.meta, delta.Meta)
			if sub, parent := extractSubagentLineage(tracked.meta); sub {
				if !tracked.subagent || (parent != "" && parent != tracked.parent) {
					needUse = true
				}
				tracked.subagent = true
				if parent != "" {
					tracked.parent = parent
				}
			}
		}
		if needUse {
			t.emitToolUse(tracked, emit)
		}
		t.maybeEmitToolResult(tracked, emit)

	case update.UsageUpdate != nil:
		if update.UsageUpdate.Cost != nil && update.UsageUpdate.Cost.Amount > 0 {
			t.sessionCostUSD = update.UsageUpdate.Cost.Amount
		}
		if update.UsageUpdate.Used > 0 {
			emit(EventContext, ContextPayload{Tokens: int64(update.UsageUpdate.Used)})
		}
	}
}

func (t *acpTranslator) emitPlan(plan *acp.SessionUpdatePlan, emit emitter) {
	todos := make([]map[string]any, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		status := string(entry.Status)
		switch entry.Status {
		case acp.PlanEntryStatusPending, acp.PlanEntryStatusInProgress, acp.PlanEntryStatusCompleted:
		default:
			status = "pending"
		}
		todos = append(todos, map[string]any{
			"content": entry.Content,
			"status":  status,
		})
	}
	raw, _ := json.Marshal(map[string]any{"todos": todos})
	summary := fmt.Sprintf("%d tasks", len(todos))
	if len(todos) == 1 {
		summary = "1 task"
	}
	emit(EventToolUse, ToolUsePayload{
		ID:      "acp-plan",
		Name:    "TodoWrite",
		Summary: summary,
		Input:   toolInput(raw),
	})
	emit(EventToolResult, ToolResultPayload{
		ID:   "acp-plan",
		Name: "TodoWrite",
		OK:   true,
	})
}

func (t *acpTranslator) ensureTrackedTool(id string) *acpTrackedTool {
	tracked := t.tools[id]
	if tracked == nil {
		tracked = &acpTrackedTool{id: id}
		t.tools[id] = tracked
		t.toolOrder = append(t.toolOrder, id)
	}
	return tracked
}

func (t *acpTranslator) ensureSubagent(id string) *acpSubagentState {
	sub := t.subagents[id]
	if sub == nil {
		sub = &acpSubagentState{id: id}
		t.subagents[id] = sub
	}
	return sub
}

func normalizeSubagentStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "failed", "error":
		return "failed"
	case "cancelled", "canceled", "stopped", "aborted":
		return "stopped"
	case "running", "in_progress":
		return "running"
	default:
		return "completed"
	}
}

func firstPositiveInt64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func buildTaskUsage(tokens, toolUses, durationMs int64) *TaskUsage {
	if tokens <= 0 && toolUses <= 0 && durationMs <= 0 {
		return nil
	}
	return &TaskUsage{
		Tokens:     tokens,
		ToolUses:   toolUses,
		DurationMs: durationMs,
	}
}

func (t *acpTranslator) linkSubagentByDescription(sub *acpSubagentState) *acpTrackedTool {
	if sub == nil {
		return nil
	}
	if sub.toolCallID != "" {
		return t.tools[sub.toolCallID]
	}
	wantDesc := strings.TrimSpace(sub.description)
	if wantDesc == "" {
		return nil
	}
	for _, id := range t.toolOrder {
		tool := t.tools[id]
		if tool == nil || tool.subagentID != "" {
			continue
		}
		name, desc, summary, _ := acpToolPresentationForTracked(tool)
		if name != "Task" && name != "Agent" {
			continue
		}
		taskDesc := firstNonEmpty(desc, summary)
		if taskDesc == wantDesc {
			tool.subagentID = sub.id
			sub.toolCallID = tool.id
			return tool
		}
	}
	return nil
}

func (t *acpTranslator) bindToolToSubagent(tool *acpTrackedTool, desc, summary string, resMeta subagentResultMeta) *acpSubagentState {
	if tool == nil {
		return nil
	}
	if resMeta.subagentID != "" {
		tool.subagentID = resMeta.subagentID
		sub := t.ensureSubagent(resMeta.subagentID)
		sub.toolCallID = tool.id
		if resMeta.subagentType != "" && sub.subagentType == "" {
			sub.subagentType = resMeta.subagentType
		}
		return sub
	}
	if tool.subagentID != "" {
		sub := t.ensureSubagent(tool.subagentID)
		sub.toolCallID = tool.id
		return sub
	}
	taskDesc := firstNonEmpty(desc, summary)
	if taskDesc == "" {
		return nil
	}
	for _, sub := range t.subagents {
		if sub.toolCallID == "" && strings.TrimSpace(sub.description) == taskDesc {
			tool.subagentID = sub.id
			sub.toolCallID = tool.id
			return sub
		}
	}
	return nil
}

// HandleExtensionUpdate translates Grok Build _x.ai/session/update notifications
// for subagent lifecycle and usage into Task/ToolResult events.
func (t *acpTranslator) HandleExtensionUpdate(raw json.RawMessage, emit emitter) {
	if len(raw) == 0 {
		return
	}
	var note struct {
		SessionUpdate  string `json:"sessionUpdate"`
		SubagentID     string `json:"subagent_id"`
		ChildSessionID string `json:"child_session_id"`
		SubagentType   string `json:"subagent_type"`
		Description    string `json:"description"`
		Model          string `json:"model"`
		Status         string `json:"status"`
		ToolCalls      int64  `json:"tool_calls"`
		DurationMs     int64  `json:"duration_ms"`
		TokensUsed     int64  `json:"tokens_used"`
		Output         string `json:"output"`
	}
	if err := json.Unmarshal(raw, &note); err != nil {
		return
	}
	switch note.SessionUpdate {
	case "subagent_spawned":
		if strings.TrimSpace(note.SubagentID) == "" {
			return
		}
		sub := t.ensureSubagent(strings.TrimSpace(note.SubagentID))
		sub.childSessionID = firstNonEmpty(note.ChildSessionID, sub.childSessionID)
		sub.subagentType = firstNonEmpty(note.SubagentType, sub.subagentType)
		sub.description = firstNonEmpty(note.Description, sub.description)
		sub.model = firstNonEmpty(note.Model, sub.model)
		if !sub.finished && sub.status == "" {
			sub.status = "running"
		}
		tool := t.linkSubagentByDescription(sub)
		if tool != nil && tool.emitted && !tool.completed && sub.subagentType != "" {
			_, desc, summary, _ := acpToolPresentationForTracked(tool)
			emit(EventTask, TaskPayload{
				ID:          tool.id,
				Status:      "running",
				Description: firstNonEmpty(desc, summary, sub.description),
				Agent:       firstNonEmpty(subagentTypeFromFields(coerceInputMap(tool.rawInput), tool.meta), sub.subagentType),
			})
		}

	case "subagent_finished":
		if strings.TrimSpace(note.SubagentID) == "" {
			return
		}
		sub := t.ensureSubagent(strings.TrimSpace(note.SubagentID))
		sub.finished = true
		sub.status = normalizeSubagentStatus(note.Status)
		sub.childSessionID = firstNonEmpty(note.ChildSessionID, sub.childSessionID)
		sub.subagentType = firstNonEmpty(note.SubagentType, sub.subagentType)
		sub.description = firstNonEmpty(note.Description, sub.description)
		sub.tokensUsed = firstPositiveInt64(note.TokensUsed, sub.tokensUsed)
		sub.toolCalls = firstPositiveInt64(note.ToolCalls, sub.toolCalls)
		sub.durationMs = firstPositiveInt64(note.DurationMs, sub.durationMs)
		if strings.TrimSpace(note.Output) != "" {
			sub.output = note.Output
		}
		tool := t.linkSubagentByDescription(sub)
		if tool != nil && tool.completed {
			name, desc, summary, _ := acpToolPresentationForTracked(tool)
			if sub.output != "" {
				emit(EventToolResult, ToolResultPayload{
					ID:       tool.id,
					Name:     name,
					OK:       sub.status != "failed",
					Output:   truncateLines(redactSecrets(sub.output), ToolOutputLineLimit),
					Subagent: tool.subagent,
					Parent:   tool.parent,
				})
			}
			taskDesc := firstNonEmpty(desc, summary, sub.description)
			summarySource := firstNonEmpty(sub.output, extractToolOutput(tool.content, tool.rawOutput))
			emit(EventTask, TaskPayload{
				ID:          tool.id,
				Status:      sub.status,
				Description: taskDesc,
				Agent:       firstNonEmpty(subagentTypeFromFields(coerceInputMap(tool.rawInput), tool.meta), extractSubagentResultMeta(tool.rawOutput).subagentType, sub.subagentType),
				Summary:     truncateRunes(collapseSpace(redactSecrets(summarySource)), SummaryLimit),
				Usage:       buildTaskUsage(sub.tokensUsed, sub.toolCalls, sub.durationMs),
			})
		}
	}
}

func subagentTypeFromFields(fields map[string]any, meta map[string]any) string {
	for _, m := range []map[string]any{fields, meta} {
		for _, key := range []string{"subagent_type", "subagentType", "agent_type", "agentType", "agent"} {
			if val, ok := m[key].(string); ok && strings.TrimSpace(val) != "" {
				return strings.TrimSpace(val)
			}
		}
	}
	return ""
}

type subagentResultMeta struct {
	kind           string
	subagentID     string
	childSessionID string
	subagentType   string
	output         string
	message        string
	toolCalls      int64
	durationMs     int64
	tokensUsed     int64
	hasObject      bool
}

func mapString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func mapInt64(m map[string]any, keys ...string) int64 {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case int64:
			return v
		case int:
			return int64(v)
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func extractSubagentResultMeta(rawOutput any) subagentResultMeta {
	if rawOutput == nil {
		return subagentResultMeta{}
	}
	var fields map[string]any
	switch typed := rawOutput.(type) {
	case map[string]any:
		fields = typed
	default:
		if _, isStr := rawOutput.(string); isStr {
			return subagentResultMeta{}
		}
		if encoded, err := json.Marshal(rawOutput); err == nil {
			_ = json.Unmarshal(encoded, &fields)
		}
	}
	if len(fields) == 0 {
		return subagentResultMeta{}
	}
	return subagentResultMeta{
		kind:           strings.TrimSpace(mapString(fields, "type")),
		subagentID:     strings.TrimSpace(mapString(fields, "subagent_id", "subagentId")),
		childSessionID: strings.TrimSpace(mapString(fields, "child_session_id", "childSessionId")),
		subagentType:   strings.TrimSpace(mapString(fields, "subagent_type", "subagentType")),
		output:         mapString(fields, "output"),
		message:        mapString(fields, "message"),
		toolCalls:      mapInt64(fields, "tool_calls", "toolCalls", "tool_uses", "toolUses"),
		durationMs:     mapInt64(fields, "duration_ms", "durationMs"),
		tokensUsed:     mapInt64(fields, "tokens_used", "tokensUsed", "tokens"),
		hasObject:      true,
	}
}

func isBackgroundSubagent(fields map[string]any, meta map[string]any, resMeta subagentResultMeta) bool {
	if strings.EqualFold(resMeta.kind, "SubagentSpawned") {
		return true
	}
	for _, m := range []map[string]any{fields, meta} {
		for _, key := range []string{"background", "run_in_background", "subagentBackground"} {
			if b, ok := m[key].(bool); ok && b {
				return true
			}
		}
	}
	return false
}

func (t *acpTranslator) emitToolUse(tool *acpTrackedTool, emit emitter) {
	name, desc, summary, input := acpToolPresentationForTracked(tool)
	tool.emitted = true
	emit(EventToolUse, ToolUsePayload{
		ID:          tool.id,
		Name:        name,
		Summary:     summary,
		Description: desc,
		Input:       input,
		Subagent:    tool.subagent,
		Parent:      tool.parent,
	})
	if (name == "Task" || name == "Agent") && !tool.completed {
		fields := coerceInputMap(tool.rawInput)
		resMeta := extractSubagentResultMeta(tool.rawOutput)
		sub := t.bindToolToSubagent(tool, desc, summary, resMeta)
		taskDesc := firstNonEmpty(desc, summary)
		if taskDesc == "" && sub != nil {
			taskDesc = sub.description
		}
		agentType := subagentTypeFromFields(fields, tool.meta)
		if agentType == "" && sub != nil {
			agentType = sub.subagentType
		}
		tool.taskEmitted = true
		emit(EventTask, TaskPayload{
			ID:          tool.id,
			Status:      "running",
			Description: taskDesc,
			Agent:       agentType,
		})
	}
	if tool.parent != "" {
		if parentTool := t.tools[tool.parent]; parentTool != nil && parentTool.taskEmitted && !parentTool.completed {
			emit(EventTask, TaskPayload{
				ID:       parentTool.id,
				Status:   "running",
				LastTool: name,
			})
		}
	}
}

func (t *acpTranslator) maybeEmitToolResult(tool *acpTrackedTool, emit emitter) {
	if tool.completed {
		return
	}
	if tool.status != acp.ToolCallStatusCompleted && tool.status != acp.ToolCallStatusFailed {
		return
	}
	tool.completed = true
	name, desc, summary, _ := acpToolPresentationForTracked(tool)
	output := extractToolOutput(tool.content, tool.rawOutput)
	cleanOut := truncateLines(redactSecrets(output), ToolOutputLineLimit)
	ok := tool.status != acp.ToolCallStatusFailed
	emit(EventToolResult, ToolResultPayload{
		ID:       tool.id,
		Name:     name,
		OK:       ok,
		Output:   cleanOut,
		Subagent: tool.subagent,
		Parent:   tool.parent,
	})
	if tool.taskEmitted || name == "Task" || name == "Agent" {
		fields := coerceInputMap(tool.rawInput)
		resMeta := extractSubagentResultMeta(tool.rawOutput)
		sub := t.bindToolToSubagent(tool, desc, summary, resMeta)
		taskDesc := firstNonEmpty(desc, summary)
		if taskDesc == "" && sub != nil {
			taskDesc = sub.description
		}
		var subAgentType string
		var subTokens, subToolCalls, subDuration int64
		var subOutput string
		var subFinished bool
		var subStatus string
		if sub != nil {
			subAgentType = sub.subagentType
			subTokens = sub.tokensUsed
			subToolCalls = sub.toolCalls
			subDuration = sub.durationMs
			subOutput = sub.output
			subFinished = sub.finished
			subStatus = sub.status
		}
		agentType := firstNonEmpty(subagentTypeFromFields(fields, tool.meta), resMeta.subagentType, subAgentType)
		isBg := isBackgroundSubagent(fields, tool.meta, resMeta)
		if isBg && ok && !subFinished && !strings.EqualFold(resMeta.kind, "SubagentCompleted") {
			emit(EventTask, TaskPayload{
				ID:          tool.id,
				Status:      "running",
				Description: taskDesc,
				Agent:       agentType,
			})
			return
		}
		status := "completed"
		if !ok {
			status = "failed"
		} else if subFinished && subStatus != "" {
			status = subStatus
		}
		tokens := firstPositiveInt64(resMeta.tokensUsed, subTokens)
		toolUses := firstPositiveInt64(resMeta.toolCalls, subToolCalls)
		durationMs := firstPositiveInt64(resMeta.durationMs, subDuration)
		summarySource := firstNonEmpty(output, subOutput)
		emit(EventTask, TaskPayload{
			ID:          tool.id,
			Status:      status,
			Description: taskDesc,
			Agent:       agentType,
			Summary:     truncateRunes(collapseSpace(redactSecrets(summarySource)), SummaryLimit),
			Usage:       buildTaskUsage(tokens, toolUses, durationMs),
		})
	}
}

func hasDiffContent(items []acp.ToolCallContent) bool {
	for _, item := range items {
		if item.Diff != nil {
			return true
		}
	}
	return false
}

func acpToolPresentation(kind acp.ToolKind, title string, rawInput any, locations []acp.ToolCallLocation, content []acp.ToolCallContent) (name, description, summary string, input json.RawMessage) {
	return acpToolPresentationWithMeta(kind, title, rawInput, locations, content, nil)
}

func acpToolPresentationForTracked(tool *acpTrackedTool) (name, description, summary string, input json.RawMessage) {
	if tool == nil {
		return "", "", "", nil
	}
	name, description, summary, input = acpToolPresentationWithMetaCanonical(tool.kind, tool.title, tool.rawInput, tool.locations, tool.content, tool.meta, tool.canonicalName)
	if isKnownCanonicalToolName(name) {
		tool.canonicalName = name
	}
	return name, description, summary, input
}

func acpToolPresentationWithMeta(kind acp.ToolKind, title string, rawInput any, locations []acp.ToolCallLocation, content []acp.ToolCallContent, meta map[string]any) (name, description, summary string, input json.RawMessage) {
	return acpToolPresentationWithMetaCanonical(kind, title, rawInput, locations, content, meta, "")
}

func acpToolPresentationWithMetaCanonical(kind acp.ToolKind, title string, rawInput any, locations []acp.ToolCallLocation, content []acp.ToolCallContent, meta map[string]any, fallbackCanonical string) (name, description, summary string, input json.RawMessage) {
	fields := coerceInputMap(rawInput)
	var diffs []acp.ToolCallContentDiff
	for _, item := range content {
		if item.Diff != nil {
			diffs = append(diffs, *item.Diff)
		}
	}
	if len(locations) > 0 && locations[0].Path != "" {
		if _, ok := fields["file_path"]; !ok {
			if _, okPath := fields["path"]; !okPath {
				fields["file_path"] = locations[0].Path
			}
		}
	}

	switch {
	case len(diffs) == 1:
		diff := diffs[0]
		fields["file_path"] = diff.Path
		if diff.OldText == nil {
			name = "Write"
			fields["content"] = diff.NewText
		} else {
			name = "Edit"
			fields["old_string"] = *diff.OldText
			fields["new_string"] = diff.NewText
		}
	case len(diffs) > 1:
		name = "MultiEdit"
		encodedDiffs := make([]map[string]any, 0, len(diffs))
		for _, diff := range diffs {
			before := ""
			if diff.OldText != nil {
				before = *diff.OldText
			}
			encodedDiffs = append(encodedDiffs, map[string]any{
				"path":   diff.Path,
				"before": before,
				"after":  diff.NewText,
			})
		}
		fields["diffs"] = encodedDiffs
		if _, ok := fields["file_path"]; !ok {
			fields["file_path"] = diffs[0].Path
		}
	default:
		name = toolNameForKindWithMeta(kind, title, fields, meta)
		if !isKnownCanonicalToolName(name) && isKnownCanonicalToolName(fallbackCanonical) {
			name = fallbackCanonical
		}
		if name == "Bash" {
			if _, ok := fields["command"]; !ok && strings.TrimSpace(title) != "" && !strings.EqualFold(strings.TrimSpace(title), "bash") && !strings.EqualFold(strings.TrimSpace(title), "run_terminal_command") {
				fields["command"] = strings.TrimSpace(title)
			}
		}
		if name == "ExitPlanMode" {
			if _, hasPlan := fields["plan"]; !hasPlan {
				for _, alt := range []string{"planContent", "plan_content"} {
					if v, ok := fields[alt].(string); ok && strings.TrimSpace(v) != "" {
						fields["plan"] = v
						break
					}
				}
			}
		}
	}

	var rawBytes json.RawMessage
	if len(fields) > 0 {
		if encoded, err := json.Marshal(fields); err == nil {
			rawBytes = encoded
		}
	} else if rawInput != nil {
		if encoded, err := json.Marshal(rawInput); err == nil {
			rawBytes = encoded
		}
	}

	input = toolInput(rawBytes)
	description = toolDescription(rawBytes)
	cleanTitle := truncateRunes(redactSecrets(collapseSpace(title)), SummaryLimit)
	summary = summarizeToolInput(name, rawBytes)
	if cleanTitle != "" && !strings.EqualFold(cleanTitle, name) {
		if mapped, ok := canonicalGrokToolName(cleanTitle); !ok || mapped != name {
			if description == "" && cleanTitle != summary {
				description = cleanTitle
			} else if summary == "" {
				summary = cleanTitle
			}
		}
	}
	return name, description, summary, input
}

func isKnownCanonicalToolName(name string) bool {
	switch name {
	case "AskUserQuestion", "ExitPlanMode", "EnterPlanMode", "Task", "Agent", "Skill",
		"TodoWrite", "Bash", "Read", "Write", "Edit", "MultiEdit", "LS", "Grep", "Glob",
		"WebSearch", "WebFetch", "Delete", "Move", "Think", "SwitchMode":
		return true
	}
	return false
}

func canonicalGrokToolName(raw string) (string, bool) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", false
	}
	switch strings.ToLower(token) {
	case "ask_user_question", "askquestion", "ask_question", "askuserquestion":
		return "AskUserQuestion", true
	case "exit_plan_mode", "exitplanmode":
		return "ExitPlanMode", true
	case "enter_plan_mode", "enterplanmode":
		return "EnterPlanMode", true
	case "spawn_subagent", "spawnsubagent", "task", "subagent":
		return "Task", true
	case "agent":
		return "Agent", true
	case "skill", "use_skill", "invoke_skill":
		return "Skill", true
	case "todo_write", "todowrite":
		return "TodoWrite", true
	case "run_terminal_command", "run_terminal_cmd", "bash", "shell":
		return "Bash", true
	case "read_file", "read":
		return "Read", true
	case "write_file", "write":
		return "Write", true
	case "search_replace", "hashline_edit", "apply_patch", "edit_file", "edit":
		return "Edit", true
	case "multi_edit", "multiedit":
		return "MultiEdit", true
	case "list_dir", "list_directory", "ls":
		return "LS", true
	case "grep_search", "grep_files", "grep":
		return "Grep", true
	case "glob_file_search", "find_files", "glob":
		return "Glob", true
	case "web_search", "websearch":
		return "WebSearch", true
	case "web_fetch", "webfetch", "fetch_url":
		return "WebFetch", true
	}
	return "", false
}

func toolNameForKind(kind acp.ToolKind, title string, fields map[string]any) string {
	return toolNameForKindWithMeta(kind, title, fields, nil)
}

func toolNameForKindWithMeta(kind acp.ToolKind, title string, fields map[string]any, meta map[string]any) string {
	for _, key := range []string{"toolName", "tool_name", "name"} {
		if v, ok := meta[key].(string); ok {
			if mapped, ok := canonicalGrokToolName(v); ok {
				return mapped
			}
		}
	}
	if xaiTool, ok := meta["x.ai/tool"].(map[string]any); ok {
		if v, ok := xaiTool["name"].(string); ok {
			if mapped, ok := canonicalGrokToolName(v); ok {
				return mapped
			}
		}
		if k, ok := xaiTool["kind"].(string); ok && strings.EqualFold(strings.TrimSpace(k), "task") {
			return "Task"
		}
		if v, ok := xaiTool["label"].(string); ok {
			if mapped, ok := canonicalGrokToolName(v); ok {
				return mapped
			}
		}
	}
	if v, ok := fields["variant"].(string); ok {
		if mapped, ok := canonicalGrokToolName(v); ok {
			return mapped
		}
	}
	trimmed := strings.TrimSpace(title)
	if mapped, ok := canonicalGrokToolName(trimmed); ok {
		return mapped
	}
	if head, _, ok := strings.Cut(trimmed, ":"); ok {
		if mapped, ok := canonicalGrokToolName(head); ok {
			return mapped
		}
	}
	if _, hasQuestions := fields["questions"]; hasQuestions {
		return "AskUserQuestion"
	}
	if _, hasPlanContent := fields["planContent"]; hasPlanContent {
		return "ExitPlanMode"
	}
	if _, hasPlan := fields["plan"]; hasPlan && kind == acp.ToolKindSwitchMode {
		return "ExitPlanMode"
	}
	if _, hasTodos := fields["todos"]; hasTodos {
		return "TodoWrite"
	}
	if _, hasSkill := fields["skill"]; hasSkill && (kind == "" || kind == acp.ToolKindOther) {
		return "Skill"
	}
	if _, hasSubagent := fields["subagent_type"]; hasSubagent && (kind == "" || kind == acp.ToolKindOther) {
		return "Task"
	}
	if _, hasSubagentCamel := fields["subagentType"]; hasSubagentCamel && (kind == "" || kind == acp.ToolKindOther) {
		return "Task"
	}

	switch kind {
	case acp.ToolKindExecute:
		return "Bash"
	case acp.ToolKindRead:
		return "Read"
	case acp.ToolKindEdit:
		if _, ok := fields["old_string"]; ok {
			return "Edit"
		}
		if _, ok := fields["content"]; ok {
			return "Write"
		}
		return "Edit"
	case acp.ToolKindSearch:
		return "Grep"
	case acp.ToolKindFetch:
		return "WebFetch"
	case acp.ToolKindDelete:
		return "Delete"
	case acp.ToolKindMove:
		return "Move"
	case acp.ToolKindThink:
		return "Think"
	case acp.ToolKindSwitchMode:
		return "SwitchMode"
	}
	if trimmed != "" && !strings.ContainsAny(trimmed, " \t\n/\\:") && len(trimmed) <= 32 {
		return trimmed
	}
	return "Tool"
}

func coerceInputMap(rawInput any) map[string]any {
	out := map[string]any{}
	if rawInput == nil {
		return out
	}
	switch typed := rawInput.(type) {
	case map[string]any:
		for k, v := range typed {
			out[k] = v
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") {
			_ = json.Unmarshal([]byte(trimmed), &out)
		} else if trimmed != "" {
			out["input"] = trimmed
		}
	default:
		if encoded, err := json.Marshal(rawInput); err == nil {
			_ = json.Unmarshal(encoded, &out)
		}
	}
	return out
}

func contentBlockText(block acp.ContentBlock) string {
	if block.Text != nil {
		return block.Text.Text
	}
	return ""
}

func extractToolOutput(content []acp.ToolCallContent, rawOutput any) string {
	var parts []string
	for _, item := range content {
		if item.Content != nil && item.Content.Content.Text != nil && item.Content.Content.Text.Text != "" {
			parts = append(parts, item.Content.Content.Text.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if rawOutput == nil {
		return ""
	}
	switch typed := rawOutput.(type) {
	case string:
		return typed
	default:
		if meta := extractSubagentResultMeta(rawOutput); meta.hasObject {
			if strings.EqualFold(meta.kind, "SubagentSpawned") {
				return meta.message
			}
			if meta.output != "" && (meta.subagentID != "" || strings.HasPrefix(meta.kind, "Subagent")) {
				return meta.output
			}
		}
		encoded, err := json.MarshalIndent(rawOutput, "", "  ")
		if err != nil {
			return ""
		}
		if string(encoded) == "null" || string(encoded) == "{}" || string(encoded) == "[]" {
			return ""
		}
		return string(encoded)
	}
}

const (
	// ToolInputStringLimit bounds each string in a forwarded tool input, such
	// as the content of a written file.
	ToolInputStringLimit = 20000
	// toolInputLimit drops an input that is still this large once its strings
	// are cut: a call with thousands of arguments is not worth storing.
	toolInputLimit = 256 << 10
)

// toolInput is the tool call's input as the page may see it: sensitive keys
// masked, secrets in free text redacted, every string capped.
func toolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil
	}
	if fields, ok := value.(map[string]any); ok {
		redactMap(fields)
	}
	value = capStrings(value)
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > toolInputLimit {
		return nil
	}
	return encoded
}

func capStrings(value any) any {
	switch typed := value.(type) {
	case string:
		return truncateRunes(redactSecrets(typed), ToolInputStringLimit)
	case map[string]any:
		for key, item := range typed {
			typed[key] = capStrings(item)
		}
	case []any:
		for i, item := range typed {
			typed[i] = capStrings(item)
		}
	}
	return value
}

// toolDescription is the input's "description" field, when the tool has one.
func toolDescription(input json.RawMessage) string {
	var fields struct {
		Description string `json:"description"`
	}
	if len(input) == 0 || json.Unmarshal(input, &fields) != nil {
		return ""
	}
	return truncateRunes(redactSecrets(collapseSpace(fields.Description)), SummaryLimit)
}

// summarizeToolInput renders the argument that best identifies a call.
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err != nil {
		return truncateRunes(redactSecrets(string(input)), SummaryLimit)
	}
	redactMap(fields)
	for _, key := range []string{"command", "file_path", "path", "skill", "url", "pattern", "query", "description", "subagent_type", "prompt", "notebook_path"} {
		if value, ok := fields[key]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return truncateRunes(collapseSpace(text), SummaryLimit)
			}
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return truncateRunes(redactSecrets(string(encoded)), SummaryLimit)
}

var sensitiveKey = regexp.MustCompile(`(?i)(token|key|secret|password|passwd|authorization|credential|cookie)`)

// redactMap masks string values stored under a sensitive key name.
func redactMap(values map[string]any) {
	for key, value := range values {
		switch typed := value.(type) {
		case string:
			if sensitiveKey.MatchString(key) && typed != "" {
				values[key] = "***"
			}
		case map[string]any:
			redactMap(typed)
		case []any:
			for _, item := range typed {
				if nested, ok := item.(map[string]any); ok {
					redactMap(nested)
				}
			}
		}
	}
}

var (
	bearerSecret = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	envSecret    = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:token|secret|password|passwd|api_?key|authorization)[A-Za-z0-9_]*)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;}"']+)`)
)

// redactSecrets masks credentials embedded in free text, such as a shell
// command's arguments or a tool's output.
func redactSecrets(text string) string {
	if text == "" {
		return ""
	}
	text = bearerSecret.ReplaceAllString(text, "$1 ***")
	return envSecret.ReplaceAllString(text, "$1=***")
}

// truncateLines caps a tool result at limit lines.
func truncateLines(text string, limit int) string {
	if limit <= 0 || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= limit {
		return text
	}
	return strings.Join(lines[:limit], "\n") + "\n…[truncated]"
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func collapseSpace(text string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}
