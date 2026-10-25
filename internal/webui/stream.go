package webui

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
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
	// Mirrored marks a prompt read from the transcript (mirror.go): typed in
	// a terminal or imported, not a turn the page runs.
	Mirrored bool `json:"mirrored,omitempty"`
}

// InitPayload reports the Claude session a turn runs in and what it offers.
type InitPayload struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	// SlashCommands are the commands a prompt can start with.
	SlashCommands []string `json:"slashCommands,omitempty"`
	// MCPServers are the connectors Claude Code loaded, with their state.
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

// MCPServer is one MCP server as Claude Code reported it.
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
	// Description is the call's own one-line explanation (Bash and Task take
	// one), which the page shows in place of the raw command.
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
	// Usage is Claude Code's report as it stands. Its cost is the running total
	// of the Claude session, which a resumed turn continues, so it must not be
	// summed across turns; its token counts and duration are this turn's.
	Usage Usage `json:"usage"`
	// TurnUsage is what this turn alone spent. It is absent when Claude Code
	// reported nothing, such as a turn stopped before it finished.
	TurnUsage *Usage `json:"turnUsage,omitempty"`
	IsError   bool   `json:"isError"`
	// Cancelled marks a turn the user stopped; it is not a failure.
	Cancelled bool   `json:"cancelled,omitempty"`
	Message   string `json:"message,omitempty"`
	// Subscription marks a turn on a subscription (OAuth) provider: Claude
	// Code prices it from its own table, which is not what the user pays.
	Subscription bool `json:"subscription,omitempty"`
}

// TaskPayload reports the state of a task a tool call started: a sub-agent
// (Agent / Task), which Claude Code always runs in the background, or a
// background shell. The call's tool_result only says it was launched; these
// events say when the work itself is running and how it ended.
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

// emitter receives the events one stream line translates into.
type emitter func(kind string, payload any)

// translator turns Claude Code's stream-json lines into ccl serve events. It
// keeps the tool names it has seen so a tool_result can name its call.
type translator struct {
	tools map[string]string
	// streamed holds the ids of messages whose text arrived as stream events;
	// their complete assistant message repeats it and is skipped.
	streamed map[string]bool
	// background counts the live background tasks that are activity (not
	// ambient), as Claude Code last reported them.
	background int
}

// Background reports whether background work is still running. Claude Code
// waits for it, then answers its notification in another turn with its own
// result, before it exits.
func (t *translator) Background() bool { return t.background > 0 }

func newTranslator() *translator {
	return &translator{tools: make(map[string]string), streamed: make(map[string]bool)}
}

// Handle translates one line. Malformed or unrecognized lines are ignored: the
// process may write diagnostics to the same pipe.
func (t *translator) Handle(line []byte, emit emitter) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var raw struct {
		Type            string          `json:"type"`
		Subtype         string          `json:"subtype"`
		SessionID       string          `json:"session_id"`
		Model           string          `json:"model"`
		Cwd             string          `json:"cwd"`
		ParentToolUseID string          `json:"parent_tool_use_id"`
		SlashCommands   []string        `json:"slash_commands"`
		MCPServers      []MCPServer     `json:"mcp_servers"`
		IsError         bool            `json:"is_error"`
		Result          string          `json:"result"`
		DurationMs      int64           `json:"duration_ms"`
		TotalCostUSD    float64         `json:"total_cost_usd"`
		Usage           json.RawMessage `json:"usage"`
		Message         json.RawMessage `json:"message"`
		Event           json.RawMessage `json:"event"`
		RequestID       string          `json:"request_id"`
		Request         json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return
	}
	parent := strings.TrimSpace(raw.ParentToolUseID)

	switch raw.Type {
	case "system":
		if raw.Subtype != "init" {
			t.handleSystem(raw.Subtype, line, emit)
			return
		}
		emit(EventInit, InitPayload{
			SessionID:     raw.SessionID,
			Model:         raw.Model,
			Cwd:           raw.Cwd,
			SlashCommands: raw.SlashCommands,
			MCPServers:    raw.MCPServers,
		})

	case "stream_event":
		t.handleStreamEvent(raw.Event, parent, emit)

	case "assistant":
		t.handleAssistant(raw.Message, parent, emit)

	case "user":
		t.handleUser(raw.Message, parent, emit)

	case "control_request":
		t.handleControlRequest(raw.RequestID, raw.Request, emit)

	case "control_cancel_request":
		// Claude Code no longer needs this answer (the call was interrupted).
		if raw.RequestID != "" {
			emit(EventPermissionResolved, PermissionResolvedPayload{RequestID: raw.RequestID, Behavior: "cancelled"})
		}

	case "result":
		payload := ResultPayload{IsError: raw.IsError, Usage: usageFrom(raw.Usage, raw.DurationMs, raw.TotalCostUSD)}
		if raw.Subtype != "" && raw.Subtype != "success" && !raw.IsError {
			payload.Message = raw.Subtype
		}
		if payload.IsError {
			payload.Message = truncateRunes(redactSecrets(raw.Result), SummaryLimit)
		}
		emit(EventResult, payload)
	}
}

// handleSystem follows background work: the task_* events of each task a tool
// call started, and the set of live background tasks.
func (t *translator) handleSystem(subtype string, line []byte, emit emitter) {
	var raw struct {
		ToolUseID      string `json:"tool_use_id"`
		Status         string `json:"status"`
		Description    string `json:"description"`
		SubagentType   string `json:"subagent_type"`
		Summary        string `json:"summary"`
		LastToolName   string `json:"last_tool_name"`
		Ambient        bool   `json:"ambient"`
		SkipTranscript bool   `json:"skip_transcript"`
		Usage          *struct {
			TotalTokens int64 `json:"total_tokens"`
			ToolUses    int64 `json:"tool_uses"`
			DurationMs  int64 `json:"duration_ms"`
		} `json:"usage"`
		Tasks []struct {
			Ambient bool `json:"ambient"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return
	}
	if subtype == "background_tasks_changed" {
		t.background = 0
		for _, task := range raw.Tasks {
			if !task.Ambient {
				t.background++
			}
		}
		return
	}
	if subtype != "task_started" && subtype != "task_progress" && subtype != "task_notification" {
		return
	}
	if raw.ToolUseID == "" || raw.Ambient || raw.SkipTranscript {
		return
	}
	payload := TaskPayload{
		ID:          raw.ToolUseID,
		Status:      "running",
		Description: truncateRunes(redactSecrets(raw.Description), SummaryLimit),
		Agent:       raw.SubagentType,
		Summary:     truncateRunes(redactSecrets(raw.Summary), SummaryLimit),
		LastTool:    raw.LastToolName,
	}
	if subtype == "task_notification" {
		payload.Status = raw.Status
		if payload.Status != "completed" && payload.Status != "failed" {
			payload.Status = "stopped"
		}
	}
	if raw.Usage != nil {
		payload.Usage = &TaskUsage{Tokens: raw.Usage.TotalTokens, ToolUses: raw.Usage.ToolUses, DurationMs: raw.Usage.DurationMs}
	}
	emit(EventTask, payload)
}

// usageFrom decodes Claude Code's usage block, which is absent for some error
// results.
func usageFrom(data json.RawMessage, durationMs int64, costUSD float64) Usage {
	usage := Usage{DurationMs: durationMs, CostUSD: costUSD}
	if len(data) == 0 {
		return usage
	}
	var counts struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		return usage
	}
	usage.InputTokens = counts.InputTokens + counts.CacheCreationTokens + counts.CacheReadTokens
	usage.OutputTokens = counts.OutputTokens
	return usage
}

func (t *translator) handleStreamEvent(data json.RawMessage, parent string, emit emitter) {
	if len(data) == 0 {
		return
	}
	subagent := parent != ""
	var event struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens         int64 `json:"input_tokens"`
				CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message.ID != "" {
			t.streamed[event.Message.ID] = true
		}
		// A sub-agent's calls have their own context.
		usage := event.Message.Usage
		if tokens := usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens; tokens > 0 && parent == "" {
			emit(EventContext, ContextPayload{Tokens: tokens})
		}
	case "content_block_delta":
		switch {
		case event.Delta.Type == "text_delta" && event.Delta.Text != "":
			emit(EventToken, TokenPayload{Text: event.Delta.Text, Subagent: subagent, Parent: parent})
		case event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "":
			emit(EventThinking, TokenPayload{Text: event.Delta.Thinking, Subagent: subagent, Parent: parent})
		}
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" && event.ContentBlock.ID != "" {
			// The full input only arrives with the assistant message; this
			// early notice lets the UI show the call immediately.
			t.tools[event.ContentBlock.ID] = event.ContentBlock.Name
			emit(EventToolUse, ToolUsePayload{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Subagent: subagent, Parent: parent})
		}
	}
}

func (t *translator) handleAssistant(data json.RawMessage, parent string, emit emitter) {
	subagent := parent != ""
	blocks := contentBlocks(data)
	// Text of a streamed message arrived already; a message that was not
	// streamed (a sub-agent's, typically) brings its text only here. Tool calls
	// always need the complete message, which carries their full input.
	streamed := t.streamed[messageID(data)]
	for _, block := range blocks {
		switch {
		case block.Type == "text" && !streamed && block.Text != "":
			emit(EventToken, TokenPayload{Text: block.Text, Subagent: subagent, Parent: parent})
			continue
		case block.Type == "thinking" && !streamed && block.Thinking != "":
			emit(EventThinking, TokenPayload{Text: block.Thinking, Subagent: subagent, Parent: parent})
			continue
		case block.Type != "tool_use":
			continue
		}
		if block.ID != "" {
			t.tools[block.ID] = block.Name
		}
		emit(EventToolUse, ToolUsePayload{
			ID:          block.ID,
			Name:        block.Name,
			Summary:     summarizeToolInput(block.Name, block.Input),
			Description: toolDescription(block.Input),
			Input:       toolInput(block.Input),
			Subagent:    subagent,
			Parent:      parent,
		})
	}
}

func messageID(message json.RawMessage) string {
	var envelope struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(message, &envelope)
	return envelope.ID
}

func (t *translator) handleUser(data json.RawMessage, parent string, emit emitter) {
	for _, block := range contentBlocks(data) {
		if block.Type != "tool_result" {
			continue
		}
		emit(EventToolResult, ToolResultPayload{
			ID:       block.ToolUseID,
			Name:     t.tools[block.ToolUseID],
			OK:       !block.IsError,
			Output:   truncateLines(redactSecrets(block.Result), ToolOutputLineLimit),
			Subagent: parent != "",
			Parent:   parent,
		})
	}
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	IsError   bool            `json:"is_error"`
	// Content is raw because a tool_result's content is either a string or an
	// array of blocks; Result holds the flattened text.
	Content json.RawMessage `json:"content"`
	Result  string          `json:"-"`
}

// contentBlocks decodes message.content, which may be a plain string or an
// array of blocks.
func contentBlocks(message json.RawMessage) []contentBlock {
	if len(message) == 0 {
		return nil
	}
	var envelope struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil || len(envelope.Content) == 0 {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(envelope.Content, &blocks); err != nil {
		return nil
	}
	out := make([]contentBlock, 0, len(blocks))
	for _, item := range blocks {
		var block contentBlock
		if err := json.Unmarshal(item, &block); err != nil {
			continue
		}
		// A tool_result's content is a string, or a block array of text and
		// images when the tool returned more than text.
		if block.Type == "tool_result" {
			block.Result = toolResultText(item)
		}
		out = append(out, block)
	}
	return out
}

// toolResultText flattens a tool_result block's content to text.
func toolResultText(block json.RawMessage) string {
	var raw struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(block, &raw); err != nil || len(raw.Content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw.Content, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return string(raw.Content)
	}
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			lines = append(lines, part.Text)
		}
	}
	return strings.Join(lines, "\n")
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
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "query", "description", "prompt", "notebook_path"} {
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
