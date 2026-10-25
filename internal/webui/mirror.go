package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A conversation's Claude session is also continued outside the page:
// `ccl --resume` in a terminal appends to the same transcript. The mirror
// follows that transcript, and the sub-agent transcripts beside it, and
// appends what was added there as events, so the page shows it. Importing a
// transcript is the same replay from its start.
//
// The mirror pauses while the page runs a turn itself: that turn's events come
// from its own stream, and what it wrote to the transcript is skipped once it
// ends.

// mirrorInterval is how often an open stream checks the transcript.
const mirrorInterval = 2 * time.Second

// mirrorIdle is how long a transcript must have been quiet for its last turn
// to count as over when it recorded no end (print mode records none).
const mirrorIdle = 2 * time.Minute

// mirrorState is how far a conversation has followed its transcripts, kept in
// mirror.json beside its events.
type mirrorState struct {
	// Offset is how much of the conversation's transcript has been read.
	Offset int64 `json:"offset"`
	// Agents is how much of each sub-agent's transcript has been read, by the
	// tool call that started it; Running is which of them still work.
	Agents  map[string]int64 `json:"agents,omitempty"`
	Running map[string]bool  `json:"running,omitempty"`
	// Spent is each sub-agent's output tokens so far.
	Spent map[string]*agentSpend `json:"spent,omitempty"`
	// Open marks a turn whose end has not been read yet; Last is the time of
	// the latest line read.
	Open bool      `json:"open,omitempty"`
	Last time.Time `json:"last,omitempty"`
	// Paused is set while the page runs a turn of its own.
	Paused bool `json:"paused,omitempty"`
}

// agentSpend sums a sub-agent's output tokens. Claude Code writes each content
// block of a message as its own line with the message's usage, so a message
// counts once, with its largest report.
type agentSpend struct {
	Tokens  int64  `json:"tokens"`
	Message string `json:"message,omitempty"`
	Counted int64  `json:"counted,omitempty"`
}

func (a *agentSpend) add(message string, tokens int64) {
	if message != a.Message {
		a.Message, a.Counted = message, 0
	}
	if tokens > a.Counted {
		a.Tokens += tokens - a.Counted
		a.Counted = tokens
	}
}

func (s *Session) loadMirror() (*mirrorState, bool) {
	data, err := os.ReadFile(filepath.Join(s.dir, "mirror.json"))
	if err != nil {
		return &mirrorState{}, false
	}
	var state mirrorState
	if json.Unmarshal(data, &state) != nil {
		return &mirrorState{}, false
	}
	return &state, true
}

func (s *Session) saveMirror(state *mirrorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "mirror.json")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// transcriptOf finds the transcript of a conversation's Claude session: in
// its worktree's project directory when it has one, else its workspace's.
func (s *Server) transcriptOf(meta Meta) string {
	if meta.ClaudeSessionID == "" {
		return ""
	}
	workspace, ok := s.workspaces.byPath(meta.Workspace)
	if !ok {
		return ""
	}
	candidates := []Workspace{workspace}
	if meta.Worktree != nil {
		if tree, err := s.worktreeWorkspace(workspace, meta.Worktree.Name); err == nil {
			candidates = append([]Workspace{tree}, candidates...)
		}
	}
	for _, candidate := range candidates {
		if path, err := claudeTranscript(candidate, meta.ClaudeSessionID); err == nil {
			return path
		}
	}
	return ""
}

// syncMirror appends what was added to the conversation's transcripts since
// it last looked. A conversation it has not followed before starts at their
// current end: its history is already shown.
func (s *Server) syncMirror(session *Session) {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	if s.timeline.Running(session.ID()) {
		return
	}
	transcript := s.transcriptOf(session.Meta())
	if transcript == "" {
		return
	}
	state, ok := session.loadMirror()
	if !ok || state.Paused {
		// What the page's own turn wrote is already shown.
		_ = session.saveMirror(mirrorAtEnd(transcript))
		return
	}
	before, _ := json.Marshal(state)
	replay := newTranscriptReplay(session, state, transcript)
	if err := replay.run(transcript); err != nil {
		return
	}
	// Claude Code records no turn end in print mode (`-p`).
	if state.Open && time.Since(state.Last) > mirrorIdle {
		replay.closeTurn(0)
	}
	if replay.turns > 0 {
		_, _ = session.UpdateMeta(func(meta *Meta) { meta.Turns += replay.turns })
	}
	if after, _ := json.Marshal(state); !bytes.Equal(before, after) {
		_ = session.saveMirror(state)
	}
}

// pauseMirror is called before the page starts a turn: what the terminal
// added first is caught up, then the turn's own writes are left to it.
func (s *Server) pauseMirror(session *Session) {
	s.syncMirror(session)
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	state, _ := session.loadMirror()
	state.Paused = true
	_ = session.saveMirror(state)
}

// followTranscript keeps a conversation's mirror current while a stream of it
// is open.
func (s *Server) followTranscript(ctx context.Context, session *Session) {
	s.syncMirror(session)
	ticker := time.NewTicker(mirrorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncMirror(session)
		}
	}
}

// mirrorAtEnd is a mirror that has read everything the transcripts hold now.
func mirrorAtEnd(transcript string) *mirrorState {
	state := &mirrorState{Offset: fileSize(transcript), Agents: map[string]int64{}}
	agents, _ := subagentTranscripts(transcript)
	for call, path := range agents {
		state.Agents[call] = fileSize(path)
	}
	return state
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// importTranscript replays a whole transcript into a new conversation and
// leaves its mirror at the end, to follow what is added later.
func importTranscript(path string, session *Session) (int, error) {
	state := &mirrorState{}
	replay := newTranscriptReplay(session, state, path)
	err := replay.run(path)
	if state.Open && time.Since(state.Last) > mirrorIdle {
		replay.closeTurn(0)
	}
	if saveErr := session.saveMirror(state); err == nil {
		err = saveErr
	}
	return replay.turns, err
}

// transcriptReplay turns transcript lines into events. Sidechain lines belong
// to sub-agents, whose own transcripts sit beside the conversation's
// (subagentTranscripts): each is replayed under the Agent / Task call that
// started it.
type transcriptReplay struct {
	session *Session
	state   *mirrorState
	// agents maps a tool call to the transcript of the sub-agent it started,
	// and calls a sub-agent's id to that call.
	agents map[string]string
	calls  map[string]string
	tools  map[string]string
	// stops maps a TaskStop call to the sub-agent it stops.
	stops map[string]string
	// turns counts the prompts replayed.
	turns int
}

func newTranscriptReplay(session *Session, state *mirrorState, transcript string) *transcriptReplay {
	if state.Agents == nil {
		state.Agents = map[string]int64{}
	}
	if state.Running == nil {
		state.Running = map[string]bool{}
	}
	if state.Spent == nil {
		state.Spent = map[string]*agentSpend{}
	}
	agents, calls := subagentTranscripts(transcript)
	return &transcriptReplay{
		session: session,
		state:   state,
		agents:  agents,
		calls:   calls,
		tools:   map[string]string{},
		stops:   map[string]string{},
	}
}

// run replays what was added to the transcript, then what its sub-agents,
// which keep working after the turn that started them, added to theirs.
func (r *transcriptReplay) run(transcript string) error {
	offset, err := r.read(transcript, r.state.Offset, "")
	r.state.Offset = offset
	if err != nil {
		return err
	}
	calls := make([]string, 0, len(r.state.Agents))
	for call := range r.state.Agents {
		calls = append(calls, call)
	}
	sort.Strings(calls)
	for _, call := range calls {
		r.agent(call)
	}
	return nil
}

// agent replays what a sub-agent added to its transcript and reports whether
// it still works: it does while its latest step is a tool call or a result
// awaiting the next one, and it is done once it answered with text alone.
func (r *transcriptReplay) agent(call string) {
	path := r.agents[call]
	if path == "" {
		return
	}
	spent := r.spent(call)
	tokens := spent.Tokens
	var working *bool
	offset, _ := r.read(path, r.state.Agents[call], call, func(running bool) { working = &running })
	r.state.Agents[call] = offset
	was, known := r.state.Running[call]
	if working == nil {
		if !known {
			return
		}
		working = &was
	}
	if known && was == *working && spent.Tokens == tokens {
		return
	}
	r.state.Running[call] = *working
	status := "completed"
	if *working {
		status = "running"
	}
	task := TaskPayload{ID: call, Status: status}
	if spent.Tokens > 0 {
		task.Usage = &TaskUsage{Tokens: spent.Tokens}
	}
	r.task(task)
}

func (r *transcriptReplay) spent(call string) *agentSpend {
	spent := r.state.Spent[call]
	if spent == nil {
		spent = &agentSpend{}
		r.state.Spent[call] = spent
	}
	return spent
}

func (r *transcriptReplay) task(task TaskPayload) {
	if task.Status != "running" {
		r.state.Running[task.ID] = false
	}
	_, _ = r.session.AppendAt(EventTask, task, r.state.Last)
}

// read replays the complete lines of path after offset and returns the offset
// after them; a line still being written is read next time.
func (r *transcriptReplay) read(path string, offset int64, parent string, progress ...func(running bool)) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, nil
		}
		return offset, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReaderSize(file, 256<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return offset, nil
			}
			return offset, err
		}
		offset += int64(len(line))
		var entry transcriptLine
		if json.Unmarshal(line, &entry) != nil || entry.IsMeta || entry.IsSidechain != (parent != "") {
			continue
		}
		if running, ok := r.entry(entry, parent); ok {
			for _, report := range progress {
				report(running)
			}
		}
	}
}

// entry replays one line. For a sub-agent's line it also says whether the
// sub-agent is still working after it.
func (r *transcriptReplay) entry(entry transcriptLine, parent string) (running, ok bool) {
	subagent := parent != ""
	ts := entry.Timestamp
	if ts.IsZero() {
		ts = r.state.Last
	}
	if ts.After(r.state.Last) {
		r.state.Last = ts
	}
	switch entry.Type {
	case "system":
		// Claude Code records how long each turn took when it ends.
		if !subagent && entry.Subtype == "turn_duration" {
			r.closeTurn(entry.DurationMs)
		}
	case "user":
		// A sub-agent's prompt is the call's own input, already shown; it
		// says the sub-agent has started.
		if subagent {
			running, ok = true, true
		} else {
			if task, agent, isTask := taskNotification(entry.Message); isTask {
				if task.ID == "" {
					task.ID = r.calls[agent]
				}
				if task.ID != "" {
					r.task(task)
				}
			} else if text := promptText(entry.Message); text != "" {
				r.closeTurn(0)
				r.turns++
				r.state.Open = true
				_, _ = r.session.AppendAt(EventUser, UserPayload{Text: text, Mirrored: true}, ts)
			}
		}
		for _, block := range contentBlocks(entry.Message) {
			if block.Type != "tool_result" {
				continue
			}
			running, ok = true, true
			_, _ = r.session.AppendAt(EventToolResult, ToolResultPayload{
				ID: block.ToolUseID, Name: r.tools[block.ToolUseID], OK: !block.IsError,
				Output:   truncateLines(redactSecrets(block.Result), ToolOutputLineLimit),
				Subagent: subagent, Parent: parent,
			}, ts)
			if agent := r.stops[block.ToolUseID]; agent != "" && !block.IsError {
				if call := r.calls[agent]; call != "" {
					r.task(TaskPayload{ID: call, Status: "stopped"})
				}
			}
		}
	case "assistant":
		if !subagent {
			r.state.Open = true
		} else {
			var message struct {
				ID    string `json:"id"`
				Usage struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(entry.Message, &message) == nil {
				r.spent(parent).add(message.ID, message.Usage.OutputTokens)
			}
		}
		ok = true
		for _, block := range contentBlocks(entry.Message) {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) != "" {
					_, _ = r.session.AppendAt(EventToken, TokenPayload{Text: block.Text + "\n\n", Subagent: subagent, Parent: parent}, ts)
				}
			case "thinking":
				if strings.TrimSpace(block.Thinking) != "" {
					_, _ = r.session.AppendAt(EventThinking, TokenPayload{Text: block.Thinking, Subagent: subagent, Parent: parent}, ts)
				}
			case "tool_use":
				running = true
				r.tools[block.ID] = block.Name
				_, _ = r.session.AppendAt(EventToolUse, ToolUsePayload{
					ID: block.ID, Name: block.Name,
					Summary:     summarizeToolInput(block.Name, block.Input),
					Description: toolDescription(block.Input),
					Input:       toolInput(block.Input),
					Subagent:    subagent, Parent: parent,
				}, ts)
				if block.Name == "TaskStop" {
					var input struct {
						TaskID string `json:"task_id"`
					}
					if json.Unmarshal(block.Input, &input) == nil && input.TaskID != "" {
						r.stops[block.ID] = input.TaskID
					}
				}
				// A sub-agent's transcript may not exist yet; it is read once
				// it does.
				if _, seen := r.state.Agents[block.ID]; !seen && (block.Name == "Agent" || block.Name == "Task") {
					r.state.Agents[block.ID] = 0
					r.agent(block.ID)
				}
			}
		}
	}
	return running, ok
}

func (r *transcriptReplay) closeTurn(durationMs int64) {
	if r.state.Open {
		_, _ = r.session.AppendAt(EventResult, ResultPayload{Usage: Usage{DurationMs: durationMs}}, r.state.Last)
		r.state.Open = false
	}
}

// subagentTranscripts finds the sub-agent transcripts of a conversation, which
// Claude Code keeps in <id>/subagents/agent-<agent>.jsonl beside <id>.jsonl,
// each with a .meta.json naming the tool call that started it. It returns the
// transcripts by call, and the calls by agent id.
func subagentTranscripts(path string) (map[string]string, map[string]string) {
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	agents, calls := map[string]string{}, map[string]string{}
	for _, metaPath := range metas {
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta struct {
			ToolUseID string `json:"toolUseId"`
		}
		transcript := strings.TrimSuffix(metaPath, ".meta.json") + ".jsonl"
		if json.Unmarshal(data, &meta) != nil || meta.ToolUseID == "" {
			continue
		}
		calls[strings.TrimSuffix(strings.TrimPrefix(filepath.Base(transcript), "agent-"), ".jsonl")] = meta.ToolUseID
		if info, err := os.Stat(transcript); err == nil && info.Mode().IsRegular() {
			agents[meta.ToolUseID] = transcript
		}
	}
	return agents, calls
}

var notificationTag = regexp.MustCompile(`(?s)<(task-id|tool-use-id|status|summary)>(.*?)</(?:task-id|tool-use-id|status|summary)>`)

// taskNotification reads the note Claude Code records when background work
// ends, which the transcript keeps as a user line. A sub-agent woken again
// names only its own id (agent) when it stops.
func taskNotification(message json.RawMessage) (task TaskPayload, agent string, ok bool) {
	var envelope struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(message, &envelope) != nil || !strings.HasPrefix(strings.TrimSpace(envelope.Content), "<task-notification>") {
		return task, "", false
	}
	task.Status = "stopped"
	for _, match := range notificationTag.FindAllStringSubmatch(envelope.Content, -1) {
		value := strings.TrimSpace(match[2])
		switch match[1] {
		case "task-id":
			agent = value
		case "tool-use-id":
			task.ID = value
		case "status":
			if value == "completed" || value == "failed" {
				task.Status = value
			}
		case "summary":
			task.Summary = truncateRunes(redactSecrets(value), SummaryLimit)
		}
	}
	return task, agent, true
}
