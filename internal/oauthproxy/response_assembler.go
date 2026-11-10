package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	assemblerMaxRetainedBytes = 64 << 20
	assemblerMaxBlocks        = 4096
	assemblerMaxToolCalls     = 1024
)

// Assembler block kinds. A block becomes one Responses output item.
const (
	blockText     = "text"
	blockThinking = "thinking"
	blockToolUse  = "tool_use"
	// blockOpaque is reasoning the upstream returned only in encrypted form.
	blockOpaque = "redacted_thinking"
)

// Stop reasons a backend records on the assembler. They decide whether the
// response completes or is reported incomplete.
const (
	stopEndTurn   = "end_turn"
	stopToolUse   = "tool_use"
	stopMaxTokens = "max_tokens"
	stopRefusal   = "refusal"
)

// assemblerBlock is one output item under assembly.
type assemblerBlock struct {
	Type string
	// ItemID is the Responses item id.
	ItemID   string
	Text     string
	Thinking string
	// Signature is the opaque payload a backend attaches to its reasoning so a
	// later request can replay it; it travels as encrypted_content.
	Signature string
	// Data is reasoning the upstream returned only in encrypted form.
	Data string
	// ID, Name and Arguments describe a tool call.
	ID        string
	Name      string
	Arguments string
	spec      toolSpec
	closed    bool
}

type assemblerToolAccumulator struct {
	name  string
	input strings.Builder
}

// assemblerBlockBuffer accumulates the streamed text/thinking of one block.
// Appending to a strings.Builder keeps assembly linear; concatenating onto the
// block's string field would copy the whole block on every delta.
type assemblerBlockBuffer struct {
	text     strings.Builder
	thinking strings.Builder
}

// responseAssembler turns the text, reasoning and tool calls a backend decodes
// from its upstream into one Responses response: server-sent events when it has
// a writer, and the final response object either way.
type responseAssembler struct {
	request          *adapterRequest
	writer           http.ResponseWriter
	flusher          http.Flusher
	responseID       string
	createdAt        int64
	sequence         int
	blocks           []assemblerBlock
	buffers          []*assemblerBlockBuffer
	eventBuffer      bytes.Buffer
	encoder          *json.Encoder
	activeIndex      int
	activeType       string
	outputTokens     int
	retainedBytes    int
	contextTokens    int
	inputUsageKnown  bool
	cacheReadTokens  int
	cacheWriteTokens int
	hasToolUse       bool
	tools            map[string]*assemblerToolAccumulator
	creditUsage      float64
	creditUnit       string
	creditPlural     string
	stopReason       string
	started          bool
	finished         bool
	failed           bool
	nativeReasoning  bool
	inlineState      string
	inlineBuffer     string
}

func newResponseAssembler(request *adapterRequest, writer http.ResponseWriter) *responseAssembler {
	assembler := &responseAssembler{
		request:     request,
		writer:      writer,
		responseID:  "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		createdAt:   time.Now().Unix(),
		activeIndex: -1,
		tools:       make(map[string]*assemblerToolAccumulator),
	}
	if writer != nil {
		assembler.flusher, _ = writer.(http.Flusher)
	}
	assembler.encoder = json.NewEncoder(&assembler.eventBuffer)
	assembler.encoder.SetEscapeHTML(false)
	return assembler
}

func assemblerItemID(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// appendBlock registers a new block and its accumulation buffer, keeping both
// slices index-aligned.
func (a *responseAssembler) appendBlock(block assemblerBlock) int {
	index := len(a.blocks)
	a.blocks = append(a.blocks, block)
	a.buffers = append(a.buffers, &assemblerBlockBuffer{})
	return index
}

// contentBlocks materializes the accumulated text/thinking into a copy of the
// block list. Call it only when a full response body is needed.
func (a *responseAssembler) contentBlocks() []assemblerBlock {
	blocks := make([]assemblerBlock, len(a.blocks))
	copy(blocks, a.blocks)
	for index := range blocks {
		if index >= len(a.buffers) || a.buffers[index] == nil {
			continue
		}
		if buffer := a.buffers[index]; buffer.text.Len() > 0 {
			blocks[index].Text = buffer.text.String()
		}
		if buffer := a.buffers[index]; buffer.thinking.Len() > 0 {
			blocks[index].Thinking = buffer.thinking.String()
		}
	}
	return blocks
}

// blockText returns the text or thinking accumulated for a block so far.
func (a *responseAssembler) blockContent(index int) string {
	block := a.blocks[index]
	if block.Type == blockThinking {
		if a.buffers[index].thinking.Len() > 0 {
			return a.buffers[index].thinking.String()
		}
		return block.Thinking
	}
	if a.buffers[index].text.Len() > 0 {
		return a.buffers[index].text.String()
	}
	return block.Text
}

// item renders one block as a Responses output item.
func (a *responseAssembler) item(index int, complete bool) map[string]any {
	block := a.blocks[index]
	status := "in_progress"
	if complete {
		status = "completed"
	}
	switch block.Type {
	case blockToolUse:
		switch block.spec.Kind {
		case "tool_search":
			arguments := map[string]any{}
			_ = decodeProtocolJSON([]byte(block.Arguments), &arguments)
			return map[string]any{"id": "tsc_" + block.ID, "type": "tool_search_call", "call_id": block.ID,
				"execution": "client", "arguments": arguments, "status": status}
		case "custom":
			var input map[string]any
			_ = decodeProtocolJSON([]byte(block.Arguments), &input)
			value, _ := input["input"].(string)
			item := map[string]any{"id": block.ItemID, "type": "custom_tool_call", "call_id": block.ID,
				"name": block.Name, "input": value, "status": status}
			if block.spec.Namespace != "" {
				item["namespace"] = block.spec.Namespace
			}
			return item
		}
		item := map[string]any{"id": block.ItemID, "type": "function_call", "call_id": block.ID,
			"name": block.Name, "arguments": block.Arguments, "status": status}
		if block.spec.Namespace != "" {
			item["namespace"] = block.spec.Namespace
		}
		return item
	case blockThinking:
		item := map[string]any{"id": block.ItemID, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": a.blockContent(index)}}, "status": status}
		if block.Signature != "" {
			item["encrypted_content"] = block.Signature
		}
		return item
	case blockOpaque:
		return map[string]any{"id": block.ItemID, "type": "reasoning", "summary": []any{},
			"encrypted_content": block.Data, "status": status}
	default:
		return map[string]any{"id": block.ItemID, "type": "message", "role": "assistant", "status": status,
			"content": []any{map[string]any{"type": "output_text", "text": a.blockContent(index), "annotations": []any{}}}}
	}
}

func (a *responseAssembler) outputItems() []map[string]any {
	output := make([]map[string]any, 0, len(a.blocks))
	for index := range a.blocks {
		output = append(output, a.item(index, a.blocks[index].closed))
	}
	return output
}

// envelope renders the response object for a lifecycle event or final body.
func (a *responseAssembler) envelope(status string) map[string]any {
	response := map[string]any{
		"id": a.responseID, "object": "response", "created_at": a.createdAt, "status": status,
		"model": a.request.clientModel, "output": a.outputItems(), "usage": a.usage(),
		"error": nil, "incomplete_details": nil, "parallel_tool_calls": true,
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if a.stopReason == stopRefusal {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	return response
}

func (a *responseAssembler) start() error {
	if a.started {
		return nil
	}
	a.started = true
	if err := a.emit("response.created", map[string]any{"response": a.envelope("in_progress")}); err != nil {
		return err
	}
	return a.emit("response.in_progress", map[string]any{"response": a.envelope("in_progress")})
}

func (a *responseAssembler) addAssistantContent(content string) error {
	if !a.request.thinkingEnabled || a.nativeReasoning || a.inlineState == "done" {
		return a.addText(content)
	}
	a.inlineBuffer += content
	for {
		switch a.inlineState {
		case "thinking":
			if index := strings.Index(a.inlineBuffer, "</thinking>"); index >= 0 {
				if err := a.addThinking(a.inlineBuffer[:index]); err != nil {
					return err
				}
				a.inlineBuffer = a.inlineBuffer[index+len("</thinking>"):]
				a.inlineState = "done"
				if a.inlineBuffer != "" {
					remaining := a.inlineBuffer
					a.inlineBuffer = ""
					return a.addText(remaining)
				}
				return nil
			}
			keep := partialThinkingTagSuffix(a.inlineBuffer, "</thinking>")
			safe := a.inlineBuffer[:len(a.inlineBuffer)-keep]
			a.inlineBuffer = a.inlineBuffer[len(a.inlineBuffer)-keep:]
			return a.addThinking(safe)
		default:
			if index := strings.Index(a.inlineBuffer, "<thinking>"); index >= 0 {
				before := a.inlineBuffer[:index]
				if strings.TrimSpace(before) != "" {
					if err := a.addText(before); err != nil {
						return err
					}
				}
				a.inlineBuffer = a.inlineBuffer[index+len("<thinking>"):]
				a.inlineState = "thinking"
				continue
			}
			keep := partialThinkingTagSuffix(a.inlineBuffer, "<thinking>")
			safe := a.inlineBuffer[:len(a.inlineBuffer)-keep]
			a.inlineBuffer = a.inlineBuffer[len(a.inlineBuffer)-keep:]
			return a.addText(safe)
		}
	}
}

func (a *responseAssembler) flushInlineContent() error {
	if a.inlineBuffer == "" {
		return nil
	}
	buffer := a.inlineBuffer
	a.inlineBuffer = ""
	if a.inlineState == "thinking" {
		if index := strings.Index(buffer, "</thinking>"); index >= 0 {
			if err := a.addThinking(buffer[:index]); err != nil {
				return err
			}
			a.inlineState = "done"
			return a.addText(buffer[index+len("</thinking>"):])
		}
		return a.addThinking(buffer)
	}
	return a.addText(buffer)
}

func partialThinkingTagSuffix(content, tag string) int {
	maximum := len(tag) - 1
	if len(content) < maximum {
		maximum = len(content)
	}
	for size := maximum; size > 0; size-- {
		if strings.HasSuffix(content, tag[:size]) {
			return size
		}
	}
	return 0
}

func (a *responseAssembler) retain(size int) error {
	if a.retainedBytes+size > assemblerMaxRetainedBytes {
		return fmt.Errorf("upstream response retention exceeds %d bytes", assemblerMaxRetainedBytes)
	}
	a.retainedBytes += size
	return nil
}

func (a *responseAssembler) addText(text string) error {
	if text == "" {
		return nil
	}
	if err := a.retain(len(text)); err != nil {
		return err
	}
	index, err := a.ensureBlock(blockText)
	if err != nil {
		return err
	}
	a.buffers[index].text.WriteString(text)
	a.outputTokens += estimateApproxTokens(text)
	return a.emit("response.output_text.delta", map[string]any{
		"item_id": a.blocks[index].ItemID, "output_index": index, "content_index": 0, "delta": text,
	})
}

func (a *responseAssembler) addThinking(thinking string) error {
	if thinking == "" {
		return nil
	}
	if err := a.retain(len(thinking)); err != nil {
		return err
	}
	index, err := a.ensureBlock(blockThinking)
	if err != nil {
		return err
	}
	a.buffers[index].thinking.WriteString(thinking)
	a.outputTokens += estimateApproxTokens(thinking)
	return a.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": a.blocks[index].ItemID, "output_index": index, "summary_index": 0, "delta": thinking,
	})
}

// addOpaqueReasoning records reasoning the upstream returned only in encrypted
// form, so the client can hand it back on the next request.
func (a *responseAssembler) addOpaqueReasoning(data string) error {
	if data == "" {
		return nil
	}
	if err := a.closeActive(); err != nil {
		return err
	}
	if len(a.blocks) >= assemblerMaxBlocks {
		return fmt.Errorf("upstream response exceeds %d content blocks", assemblerMaxBlocks)
	}
	if err := a.retain(len(data)); err != nil {
		return err
	}
	index := a.appendBlock(assemblerBlock{Type: blockOpaque, ItemID: assemblerItemID(cclReasoningIDPrefix), Data: data})
	if err := a.emit("response.output_item.added", map[string]any{"output_index": index, "item": a.item(index, false)}); err != nil {
		return err
	}
	a.blocks[index].closed = true
	return a.emit("response.output_item.done", map[string]any{"output_index": index, "item": a.item(index, true)})
}

// addToolUse emits one complete tool call. name is the name the upstream
// returned; it is mapped back to the client's own declaration.
func (a *responseAssembler) addToolUse(id, name, partialJSON string) error {
	if err := a.closeActive(); err != nil {
		return err
	}
	if len(a.blocks) >= assemblerMaxBlocks {
		return fmt.Errorf("upstream response exceeds %d content blocks", assemblerMaxBlocks)
	}
	if original := a.request.toolNameMap[name]; original != "" {
		name = original
	}
	if strings.TrimSpace(partialJSON) == "" {
		partialJSON = "{}"
	}
	var input map[string]any
	if err := decodeProtocolJSON([]byte(partialJSON), &input); err != nil || input == nil {
		return fmt.Errorf("upstream tool %s returned invalid JSON input", safeToolNameForError(name))
	}
	spec, declared := a.request.tools[name]
	if a.request.tools != nil && !declared {
		return fmt.Errorf("provider called a tool outside the declared tool list: %q", safeToolNameForError(name))
	}
	if !declared {
		spec = toolSpec{Name: name, Kind: "function", WireName: name}
	}
	if spec.Kind == "custom" {
		if _, ok := input["input"].(string); !ok {
			return fmt.Errorf("provider returned invalid custom tool input")
		}
	}
	if err := a.retain(len(partialJSON)); err != nil {
		return err
	}
	index := a.appendBlock(assemblerBlock{
		Type: blockToolUse, ItemID: "fc_" + id, ID: id, Name: spec.Name, Arguments: partialJSON, spec: spec,
	})
	a.hasToolUse = true
	a.outputTokens += estimateApproxTokens(partialJSON)
	fields := map[string]any{"item_id": a.blocks[index].ItemID, "output_index": index}
	switch spec.Kind {
	case "tool_search":
		// Search arguments travel in the output item itself; the client runs
		// the search and returns tool_search_output on its next request.
		if err := a.emit("response.output_item.added", map[string]any{"output_index": index, "item": a.item(index, false)}); err != nil {
			return err
		}
	case "custom":
		value, _ := input["input"].(string)
		a.blocks[index].Arguments = ""
		added := a.item(index, false)
		a.blocks[index].Arguments = partialJSON
		if err := a.emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
			return err
		}
		if err := a.emit("response.custom_tool_call_input.delta", map[string]any{
			"item_id": fields["item_id"], "output_index": index, "delta": value}); err != nil {
			return err
		}
		if err := a.emit("response.custom_tool_call_input.done", map[string]any{
			"item_id": fields["item_id"], "output_index": index, "input": value}); err != nil {
			return err
		}
	default:
		a.blocks[index].Arguments = ""
		added := a.item(index, false)
		a.blocks[index].Arguments = partialJSON
		if err := a.emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
			return err
		}
		if err := a.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": fields["item_id"], "output_index": index, "delta": partialJSON}); err != nil {
			return err
		}
		if err := a.emit("response.function_call_arguments.done", map[string]any{
			"item_id": fields["item_id"], "output_index": index, "arguments": partialJSON}); err != nil {
			return err
		}
	}
	a.blocks[index].closed = true
	return a.emit("response.output_item.done", map[string]any{"output_index": index, "item": a.item(index, true)})
}

// safeToolNameForError keeps a damaged tool name, which can carry model
// reasoning or other payload, out of diagnostics.
func safeToolNameForError(name string) string {
	if !simpleToolName(name) {
		return "<malformed tool name omitted>"
	}
	return name
}

func (a *responseAssembler) ensureBlock(blockType string) (int, error) {
	if a.activeType == blockType && a.activeIndex >= 0 {
		return a.activeIndex, nil
	}
	if err := a.closeActive(); err != nil {
		return -1, err
	}
	if len(a.blocks) >= assemblerMaxBlocks {
		return -1, fmt.Errorf("upstream response exceeds %d content blocks", assemblerMaxBlocks)
	}
	if !a.started {
		if err := a.start(); err != nil {
			return -1, err
		}
	}
	block := assemblerBlock{Type: blockType, ItemID: assemblerItemID("msg_")}
	if blockType == blockThinking {
		block.ItemID = assemblerItemID(cclReasoningIDPrefix)
	}
	index := a.appendBlock(block)
	a.activeIndex = index
	a.activeType = blockType
	added := a.item(index, false)
	if blockType == blockThinking {
		added["summary"] = []any{}
	} else {
		added["content"] = []any{}
	}
	if err := a.emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
		return -1, err
	}
	if blockType == blockThinking {
		err := a.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": block.ItemID, "output_index": index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
		return index, err
	}
	err := a.emit("response.content_part.added", map[string]any{
		"item_id": block.ItemID, "output_index": index, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	return index, err
}

func (a *responseAssembler) closeActive() error {
	if a.activeIndex < 0 {
		return nil
	}
	index := a.activeIndex
	a.activeIndex = -1
	a.activeType = ""
	block := a.blocks[index]
	content := a.blockContent(index)
	if block.Type == blockThinking {
		if err := a.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": block.ItemID, "output_index": index, "summary_index": 0, "text": content}); err != nil {
			return err
		}
		if err := a.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": block.ItemID, "output_index": index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": content}}); err != nil {
			return err
		}
	} else {
		if err := a.emit("response.output_text.done", map[string]any{
			"item_id": block.ItemID, "output_index": index, "content_index": 0, "text": content}); err != nil {
			return err
		}
		if err := a.emit("response.content_part.done", map[string]any{
			"item_id": block.ItemID, "output_index": index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}); err != nil {
			return err
		}
	}
	a.blocks[index].closed = true
	return a.emit("response.output_item.done", map[string]any{"output_index": index, "item": a.item(index, true)})
}

func (a *responseAssembler) finish() error {
	if a.finished {
		return nil
	}
	if !a.started {
		if err := a.start(); err != nil {
			return err
		}
	}
	if err := a.flushInlineContent(); err != nil {
		return err
	}
	for id, tool := range a.tools {
		if strings.TrimSpace(tool.input.String()) != "" {
			return fmt.Errorf("upstream tool %s (%s) ended before stop=true", tool.name, id)
		}
	}
	if err := a.closeActive(); err != nil {
		return err
	}
	status := a.status()
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	if err := a.emit(event, map[string]any{"response": a.envelope(status)}); err != nil {
		return err
	}
	a.finished = true
	return nil
}

// fail ends a response that cannot be completed. Once streaming has begun the
// failure is a response.failed event; before that the caller still owns the
// HTTP status and should write an error body instead.
func (a *responseAssembler) fail(errorType, message string) {
	if a.finished || a.failed {
		return
	}
	a.failed = true
	if !a.started {
		_ = a.start()
	}
	response := a.envelope("failed")
	response["error"] = map[string]any{"code": errorType, "message": message}
	_ = a.emit("response.failed", map[string]any{"response": response})
}

// resolvedStopReason reports why the assembled turn ended.
func (a *responseAssembler) resolvedStopReason() string {
	if a.stopReason != "" {
		return a.stopReason
	}
	if a.hasToolUse {
		return stopToolUse
	}
	return stopEndTurn
}

// status is the Responses status of the finished turn. Upstream token
// exhaustion is reported incomplete, never as a fabricated completion.
func (a *responseAssembler) status() string {
	switch a.resolvedStopReason() {
	case stopMaxTokens:
		return "incomplete"
	case stopRefusal:
		if len(a.blocks) == 0 {
			return "incomplete"
		}
	}
	return "completed"
}

// response is the final response object for a non-streaming request.
func (a *responseAssembler) response() map[string]any {
	for index := range a.blocks {
		a.blocks[index].closed = true
	}
	return a.envelope(a.status())
}

// tokenTotals returns the fresh-input and output token counts for this turn.
// Cached input is tracked separately in cacheReadTokens/cacheWriteTokens.
func (a *responseAssembler) tokenTotals() (input, output int) {
	input = a.request.inputTokens
	if a.inputUsageKnown || a.contextTokens > 0 {
		input = a.contextTokens
	}
	return input, a.outputTokens
}

// usage renders Responses usage. input_tokens counts every prompt token,
// cached ones included, as the Responses API reports them.
func (a *responseAssembler) usage() map[string]any {
	inputTokens, outputTokens := a.tokenTotals()
	total := inputTokens + a.cacheReadTokens + a.cacheWriteTokens
	usage := map[string]any{
		"input_tokens":          total,
		"input_tokens_details":  map[string]any{"cached_tokens": a.cacheReadTokens},
		"output_tokens":         outputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          total + outputTokens,
	}
	if a.creditUnit != "" {
		usage["credit_usage"] = a.creditUsage
		usage["credit_unit"] = a.creditUnit
		usage["credit_unit_plural"] = a.creditPlural
	}
	return usage
}

func (a *responseAssembler) emit(event string, data map[string]any) error {
	if a.writer == nil {
		return nil
	}
	data["type"] = event
	data["sequence_number"] = a.sequence
	a.sequence++
	// Serialize straight into a reused buffer: one write per event.
	a.eventBuffer.Reset()
	a.eventBuffer.WriteString("event: ")
	a.eventBuffer.WriteString(event)
	a.eventBuffer.WriteString("\ndata: ")
	if err := a.encoder.Encode(data); err != nil {
		return err
	}
	// Encode already appended the record newline; SSE needs a blank line too.
	a.eventBuffer.WriteByte('\n')
	if _, err := a.writer.Write(a.eventBuffer.Bytes()); err != nil {
		return err
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
	return nil
}
