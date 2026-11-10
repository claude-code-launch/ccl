package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAI Responses is the single protocol every loopback runtime accepts. A
// runtime whose upstream already speaks Responses forwards the request; every
// other runtime parses it into the conversation view below and renders its own
// upstream wire format from that. Chat Completions clients are adapted to
// Responses in chat_front.go, so backends never see a second inbound dialect.

// responsesWireRequest is the subset of a Responses request body CCL reads.
type responsesWireRequest struct {
	Model             string              `json:"model"`
	Instructions      json.RawMessage     `json:"instructions"`
	Input             json.RawMessage     `json:"input"`
	Tools             []map[string]any    `json:"tools"`
	ToolChoice        any                 `json:"tool_choice"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls"`
	Stream            bool                `json:"stream"`
	MaxOutputTokens   int                 `json:"max_output_tokens"`
	Reasoning         *responsesReasoning `json:"reasoning"`
	Text              *responsesText      `json:"text"`
	Temperature       *float64            `json:"temperature"`
	TopP              *float64            `json:"top_p"`
	ServiceTier       string              `json:"service_tier"`
	PromptCacheKey    string              `json:"prompt_cache_key"`
	User              string              `json:"user"`
	Stop              json.RawMessage     `json:"stop"`
}

type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type responsesText struct {
	Format    json.RawMessage `json:"format"`
	Verbosity string          `json:"verbosity"`
}

// Conversation part kinds. A part is one unit of a turn after Responses input
// items have been grouped by speaker.
const (
	partText       = "text"
	partImage      = "image"
	partToolCall   = "tool_call"
	partToolResult = "tool_result"
	partReasoning  = "reasoning"
)

// conversationPart is one piece of a turn. Only the fields of its Kind are set.
type conversationPart struct {
	Kind string
	// Text holds message text, or the readable summary of a reasoning part.
	Text string
	// MediaType and Data describe a base64 image; URL a remote one.
	MediaType string
	Data      string
	URL       string
	// CallID, Name and Arguments describe a tool call. Name is the flattened
	// declaration name and Arguments a JSON object.
	CallID    string
	Name      string
	Arguments json.RawMessage
	// Output is the content of a tool result: text and image parts.
	Output []conversationPart
	// Signature is the opaque payload a backend attached to its own reasoning
	// (Responses encrypted_content). It is only meaningful to that backend.
	Signature string
}

// imageURL renders an image part as the URL form Chat-style upstreams accept.
func (p conversationPart) imageURL() string {
	if p.URL != "" {
		return p.URL
	}
	if p.Data == "" {
		return ""
	}
	mediaType := p.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + p.Data
}

// outputText joins the text of a tool result, leaving images to the caller.
func (p conversationPart) outputText() string {
	texts := make([]string, 0, len(p.Output))
	for _, part := range p.Output {
		if part.Kind == partText && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// outputImages returns the image parts of a tool result.
func (p conversationPart) outputImages() []conversationPart {
	var images []conversationPart
	for _, part := range p.Output {
		if part.Kind == partImage {
			images = append(images, part)
		}
	}
	return images
}

// conversationTurn is a run of consecutive items from one speaker. Role is
// "user" or "assistant"; tool results belong to the user turn that follows the
// assistant turn carrying their calls.
type conversationTurn struct {
	Role  string
	Parts []conversationPart
}

// conversationTool is one callable function after flattening namespaces and
// custom tools into plain JSON-schema functions.
type conversationTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Tool choice modes.
const (
	toolChoiceAuto     = "auto"
	toolChoiceNone     = "none"
	toolChoiceRequired = "required"
	toolChoiceTool     = "tool"
)

type conversationToolChoice struct {
	Mode string
	Name string
}

// conversationFormat is a requested json_schema structured output.
type conversationFormat struct {
	Name        string
	Description string
	Strict      *bool
	Schema      json.RawMessage
}

// toolSpec records how a flattened function declaration maps back to the tool
// the client declared, so a returned call is rendered in the client's dialect.
type toolSpec struct{ Name, Namespace, Kind, WireName string }

type toolRegistry map[string]toolSpec

// conversationRequest is the parsed form of one Responses request.
type conversationRequest struct {
	Model             string
	System            string
	Turns             []conversationTurn
	Tools             []conversationTool
	ToolChoice        conversationToolChoice
	ParallelToolCalls *bool
	// Effort is the lower-case reasoning effort, or "" when the client left the
	// upstream default in place. "none" asks for reasoning to be turned off.
	Effort          string
	MaxOutputTokens int
	Stream          bool
	Format          *conversationFormat
	Temperature     *float64
	TopP            *float64
	Stop            []string
	ServiceTier     string
	CacheKey        string
	User            string
	registry        toolRegistry
	rawBytes        int
	rawTokens       int
}

// thinkingEnabled reports whether the client asked for reasoning output.
func (r *conversationRequest) thinkingEnabled() bool {
	return r.Effort != "" && r.Effort != "none"
}

// adapter builds the client-facing state shared by every converted backend.
func (r *conversationRequest) adapter(upstreamModel string) adapterRequest {
	declared := make(map[string]struct{}, len(r.Tools))
	for _, tool := range r.Tools {
		declared[tool.Name] = struct{}{}
	}
	return adapterRequest{
		upstreamModel:     upstreamModel,
		clientModel:       r.Model,
		stream:            r.Stream,
		thinkingEnabled:   r.thinkingEnabled(),
		maxTokens:         r.MaxOutputTokens,
		inputTokens:       r.rawTokens,
		declaredToolNames: declared,
		tools:             r.registry,
	}
}

// adapterRequest is the client-facing state shared by converted backends.
// Upstream-specific converted requests embed it.
type adapterRequest struct {
	upstreamModel   string
	clientModel     string
	stream          bool
	thinkingEnabled bool
	maxTokens       int
	inputTokens     int
	// toolNameMap maps a name a backend had to rewrite for its upstream back to
	// the flattened declaration name.
	toolNameMap map[string]string
	// Chat adapters bind returned names to the current declarations. nil is
	// used by other protocols that enforce their own name mapping.
	declaredToolNames map[string]struct{}
	// tools maps a flattened declaration name to the client's own tool.
	tools toolRegistry
}

func hostedResponsesTool(kind string) bool {
	switch kind {
	case "web_search", "web_search_preview", "image_generation", "code_interpreter", "file_search",
		"computer", "computer_use_preview", "tool_search", "mcp", "local_shell", "x_search", "code_execution":
		return true
	default:
		return strings.HasPrefix(kind, "web_search") || strings.HasPrefix(kind, "computer_use")
	}
}

// toolWireName is the function name a converted upstream sees. Root tools keep
// the name client instructions use; namespaced, unsafe or over-long names get
// a deterministic hash alias, and so does a root name that already ends in the
// reserved hash suffix so it cannot impersonate one.
func toolWireName(namespace, name string) string {
	if namespace == "" && simpleToolName(name) && !hasToolHashSuffix(name) {
		return name
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	var out strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out.WriteRune(c)
		} else {
			out.WriteByte('_')
		}
		if out.Len() >= 35 {
			break
		}
	}
	return out.String() + "_" + hex.EncodeToString(sum[:6])
}

func simpleToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, c := range []byte(name) {
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func hasToolHashSuffix(name string) bool {
	if len(name) < 13 || name[len(name)-13] != '_' {
		return false
	}
	for _, c := range name[len(name)-12:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

const maxConversationTools = 1024

// flattenResponsesTools turns the client's tool declarations, plus definitions
// a client-executed tool search returned in this request, into plain functions.
func flattenResponsesTools(tools []map[string]any, discovered []map[string]any) ([]conversationTool, toolRegistry, error) {
	registry := make(toolRegistry)
	var result []conversationTool
	positions := make(map[string]int)
	explicit := make(map[string]bool)
	hasSearch := false
	for _, tool := range tools {
		if tool["type"] == "tool_search" && tool["execution"] == "client" {
			hasSearch = true
		}
	}
	var add func(map[string]any, string, bool) error
	add = func(tool map[string]any, namespace string, loaded bool) error {
		kind, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		schema := tool["parameters"]
		if kind == "namespace" {
			children, _ := tool["tools"].([]any)
			for _, child := range children {
				item, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid namespaced tool")
				}
				if err := add(item, name, loaded); err != nil {
					return err
				}
			}
			return nil
		}
		// Chat-shaped declarations nest the function under "function".
		if function, ok := tool["function"].(map[string]any); ok && kind == "function" && name == "" {
			name, _ = function["name"].(string)
			description, _ = function["description"].(string)
			schema = function["parameters"]
		}
		specKind := kind
		if kind == "tool_search" && tool["execution"] == "client" {
			kind, name = "function", "tool_search"
		}
		if kind != "function" && kind != "custom" {
			// A translated provider cannot execute OpenAI-hosted tools. Client
			// tool search is adapted above; hosted declarations are dropped.
			if hostedResponsesTool(kind) {
				return nil
			}
			return fmt.Errorf("tool type %q is not supported by this CCL provider; use local function tools", kind)
		}
		if name == "" {
			return fmt.Errorf("tool name is required")
		}
		if deferred, _ := tool["defer_loading"].(bool); deferred && hasSearch && !loaded {
			return nil
		}
		wire := toolWireName(namespace, name)
		if _, exists := registry[wire]; exists {
			if !loaded {
				return fmt.Errorf("duplicate tool definition")
			}
			if explicit[wire] {
				return nil
			}
		}
		registry[wire] = toolSpec{Name: name, Namespace: namespace, Kind: specKind, WireName: wire}
		if !loaded {
			explicit[wire] = true
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if kind == "custom" {
			schema = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The complete raw input for the tool."}}, "required": []string{"input"}, "additionalProperties": false}
			if format, ok := tool["format"].(map[string]any); ok {
				raw, _ := json.Marshal(format)
				description += "\nThe input must follow this format: " + string(raw)
			}
		}
		parameters, err := json.Marshal(schema)
		if err != nil {
			return fmt.Errorf("invalid tool parameters")
		}
		definition := conversationTool{Name: wire, Description: description, Parameters: parameters}
		if position, exists := positions[wire]; exists {
			result[position] = definition
		} else {
			positions[wire] = len(result)
			result = append(result, definition)
		}
		if len(result) > maxConversationTools {
			return fmt.Errorf("too many tool definitions")
		}
		return nil
	}
	for _, tool := range tools {
		if err := add(tool, "", false); err != nil {
			return nil, nil, err
		}
	}
	for _, tool := range discovered {
		if err := add(tool, "", true); err != nil {
			return nil, nil, err
		}
	}
	return result, registry, nil
}

// discoveredResponsesTools reads only the definitions the client supplied in
// this request. Tool search is client-owned; CCL neither indexes nor runs it.
func discoveredResponsesTools(items []map[string]any) ([]map[string]any, error) {
	var result []map[string]any
	for _, item := range items {
		if item["type"] != "tool_search_output" {
			continue
		}
		if execution, _ := item["execution"].(string); execution != "" && execution != "client" {
			return nil, fmt.Errorf("only client-executed tool search can be replayed")
		}
		if item["tools"] == nil {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			return nil, fmt.Errorf("tool search output tools must be an array")
		}
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid discovered tool definition")
			}
			result = append(result, tool)
		}
	}
	return result, nil
}

// toolCallingInstructions explains the boundary between the logical names
// client instructions use and the flattened declarations a converted upstream
// sees. It is only needed when some declaration was actually rewritten.
func toolCallingInstructions(tools []conversationTool, registry toolRegistry) string {
	rewritten := false
	for _, tool := range tools {
		spec := registry[tool.Name]
		if spec.Namespace != "" || spec.Kind == "custom" || spec.Name != tool.Name {
			rewritten = true
			break
		}
	}
	if !rewritten {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\nCCL tool calling protocol for the current request:\nOnly call functions declared in the current tools list, using their complete declaration names exactly. The logical names used in instructions map to declarations as follows (instructions may prefix an unnamespaced tool with functions.; explicit namespace mappings take precedence):\n")
	for _, tool := range tools {
		spec := registry[tool.Name]
		logical := spec.Name
		if spec.Namespace != "" {
			logical = spec.Namespace + "." + logical
		}
		fmt.Fprintf(&out, "%q => %q", logical, tool.Name)
		if spec.Kind == "custom" {
			out.WriteString("; arguments must be a JSON object with an input string containing the complete raw tool input")
		}
		out.WriteByte('\n')
	}
	out.WriteString("Tools or APIs described inside an executor's documentation are not additional callable functions. Use the declared executor's documented interface to access them. Historical calls do not add tools to the current list. When no tool is needed, respond with assistant text; do not invent a tool for the final answer.")
	return out.String()
}

// cclReasoningIDPrefix marks reasoning items a converted CCL backend produced.
// Only those are replayed to a converted backend: native reasoning belongs to
// the provider that issued it.
const cclReasoningIDPrefix = "rs_ccl_"

// CompactionSummaryPrefix marks a compaction item whose payload is a readable
// summary CCL produced, as opposed to a provider's encrypted compaction.
const CompactionSummaryPrefix = "ccl1:"

// EncodeCompactionSummary wraps a readable summary as a compaction payload.
func EncodeCompactionSummary(summary string) string {
	return CompactionSummaryPrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

// DecodeCompactionSummary reads a compaction payload CCL produced.
func DecodeCompactionSummary(encoded string) (string, bool) {
	raw, ok := strings.CutPrefix(encoded, CompactionSummaryPrefix)
	if !ok || raw == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

func responsesTextValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func responsesInputItems(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"type": "message", "role": "user", "content": text}}, nil
	}
	var items []map[string]any
	if decodeProtocolJSON(raw, &items) != nil {
		return nil, fmt.Errorf("Responses input must be text or an array of items")
	}
	return items, nil
}

// responsesContentParts converts message or tool-output content (a string or
// an array of typed parts) into conversation parts.
func responsesContentParts(value any) ([]conversationPart, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		if text == "" {
			return nil, nil
		}
		return []conversationPart{{Kind: partText, Text: text}}, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Responses message content")
	}
	result := make([]conversationPart, 0, len(parts))
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Responses content part")
		}
		switch p["type"] {
		case "input_text", "output_text", "text", "summary_text":
			if text := responsesTextValue(p["text"]); text != "" {
				result = append(result, conversationPart{Kind: partText, Text: text})
			}
		case "refusal":
			if text := responsesTextValue(p["refusal"]); text != "" {
				result = append(result, conversationPart{Kind: partText, Text: text})
			}
		case "input_image", "image_url":
			source, _ := p["image_url"].(string)
			if nested, ok := p["image_url"].(map[string]any); ok {
				source, _ = nested["url"].(string)
			}
			image, err := conversationImage(source)
			if err != nil {
				return nil, err
			}
			result = append(result, image)
		default:
			return nil, fmt.Errorf("unsupported Responses content part %q", p["type"])
		}
	}
	return result, nil
}

func conversationImage(source string) (conversationPart, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return conversationPart{}, fmt.Errorf("image input requires image_url")
	}
	if !strings.HasPrefix(source, "data:") {
		return conversationPart{Kind: partImage, URL: source}, nil
	}
	meta, data, found := strings.Cut(strings.TrimPrefix(source, "data:"), ",")
	if !found || !strings.HasSuffix(meta, ";base64") {
		return conversationPart{}, fmt.Errorf("image input must use a base64 data URL")
	}
	return conversationPart{Kind: partImage, MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}, nil
}

// parseResponsesRequest decodes a Responses request into the conversation view.
func parseResponsesRequest(raw []byte) (*conversationRequest, error) {
	var wire responsesWireRequest
	if err := decodeProtocolJSON(raw, &wire); err != nil {
		return nil, fmt.Errorf("invalid Responses request: %w", err)
	}
	model := strings.TrimSpace(wire.Model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	items, err := responsesInputItems(wire.Input)
	if err != nil {
		return nil, err
	}
	discovered, err := discoveredResponsesTools(items)
	if err != nil {
		return nil, err
	}
	tools, registry, err := flattenResponsesTools(wire.Tools, discovered)
	if err != nil {
		return nil, err
	}
	request := &conversationRequest{
		Model: model, Tools: tools, registry: registry,
		ParallelToolCalls: wire.ParallelToolCalls, MaxOutputTokens: wire.MaxOutputTokens,
		Stream: wire.Stream, Temperature: wire.Temperature, TopP: wire.TopP,
		ServiceTier: strings.TrimSpace(wire.ServiceTier), CacheKey: strings.TrimSpace(wire.PromptCacheKey),
		User: strings.TrimSpace(wire.User), rawBytes: len(raw), rawTokens: estimateApproxTokensBytes(raw),
	}
	if len(wire.Instructions) > 0 && string(wire.Instructions) != "null" {
		var instructions string
		if json.Unmarshal(wire.Instructions, &instructions) != nil {
			return nil, fmt.Errorf("instructions must be a string")
		}
		request.System = instructions
	}
	if wire.Reasoning != nil {
		request.Effort = strings.ToLower(strings.TrimSpace(wire.Reasoning.Effort))
	}
	if len(wire.Stop) > 0 && string(wire.Stop) != "null" {
		var one string
		if json.Unmarshal(wire.Stop, &one) == nil {
			if one != "" {
				request.Stop = []string{one}
			}
		} else if json.Unmarshal(wire.Stop, &request.Stop) != nil {
			return nil, fmt.Errorf("stop must be a string or an array of strings")
		}
	}
	if wire.Text != nil {
		format, err := responsesStructuredFormat(wire.Text.Format)
		if err != nil {
			return nil, err
		}
		request.Format = format
	}
	if err := request.readItems(items); err != nil {
		return nil, err
	}
	if len(request.Turns) == 0 {
		return nil, fmt.Errorf("Responses input has no conversation messages")
	}
	request.System += toolCallingInstructions(tools, registry)
	choice, err := responsesToolChoice(wire.ToolChoice, registry)
	if err != nil {
		return nil, err
	}
	request.ToolChoice = choice
	return request, nil
}

func responsesStructuredFormat(raw json.RawMessage) (*conversationFormat, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var format struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Strict      *bool           `json:"strict"`
		Schema      json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, fmt.Errorf("invalid text.format: %w", err)
	}
	switch format.Type {
	case "", "text":
		return nil, nil
	case "json_schema":
	default:
		return nil, fmt.Errorf("text.format type %q is not supported by this CCL provider", format.Type)
	}
	var schema map[string]any
	if decodeProtocolJSON(format.Schema, &schema) != nil || schema == nil {
		return nil, fmt.Errorf("text.format requires an object schema")
	}
	if format.Name == "" {
		format.Name = "ccl_structured_output"
	}
	return &conversationFormat{Name: format.Name, Description: format.Description, Strict: format.Strict, Schema: format.Schema}, nil
}

func responsesToolChoice(value any, registry toolRegistry) (conversationToolChoice, error) {
	switch choice := value.(type) {
	case nil:
		return conversationToolChoice{}, nil
	case string:
		switch choice {
		case toolChoiceAuto, toolChoiceNone, toolChoiceRequired:
			return conversationToolChoice{Mode: choice}, nil
		}
		return conversationToolChoice{}, fmt.Errorf("unsupported tool_choice")
	case map[string]any:
		name, _ := choice["name"].(string)
		if function, ok := choice["function"].(map[string]any); ok && name == "" {
			name, _ = function["name"].(string)
		}
		if choice["type"] == "tool_search" {
			name = "tool_search"
		}
		namespace, _ := choice["namespace"].(string)
		wire := toolWireName(namespace, name)
		if _, ok := registry[wire]; !ok {
			return conversationToolChoice{}, fmt.Errorf("tool_choice is not in tools")
		}
		return conversationToolChoice{Mode: toolChoiceTool, Name: wire}, nil
	default:
		return conversationToolChoice{}, fmt.Errorf("unsupported tool_choice")
	}
}

func (r *conversationRequest) appendParts(role string, parts ...conversationPart) {
	if len(parts) == 0 {
		return
	}
	if last := len(r.Turns) - 1; last >= 0 && r.Turns[last].Role == role {
		r.Turns[last].Parts = append(r.Turns[last].Parts, parts...)
		return
	}
	r.Turns = append(r.Turns, conversationTurn{Role: role, Parts: parts})
}

func textPart(text string) conversationPart { return conversationPart{Kind: partText, Text: text} }

// readItems groups Responses input items into alternating turns.
func (r *conversationRequest) readItems(items []map[string]any) error {
	// A model or tool-mode switch can leave calls to tools this request no
	// longer declares. Their contents stay as history, not callable examples.
	unavailable := make(map[string]bool)
	for _, item := range items {
		switch item["type"] {
		case "function_call", "custom_tool_call", "tool_search_call":
			name, _ := item["name"].(string)
			namespace, _ := item["namespace"].(string)
			if item["type"] == "tool_search_call" {
				name, namespace = "tool_search", ""
			}
			id, _ := item["call_id"].(string)
			if _, ok := r.registry[toolWireName(namespace, name)]; !ok && id != "" {
				unavailable[id] = true
			}
		}
	}
	if len(unavailable) > 0 {
		r.System += "\nOnly tools in the current tools list may be called. Historical tool records are conversation context, not tool definitions."
	}
	for _, item := range items {
		kind, _ := item["type"].(string)
		switch kind {
		case "", "message":
			role, _ := item["role"].(string)
			content, err := responsesContentParts(item["content"])
			if err != nil {
				return err
			}
			if role == "developer" || role == "system" {
				for _, part := range content {
					if part.Kind != partText {
						return fmt.Errorf("system content must be text")
					}
					if r.System != "" {
						r.System += "\n"
					}
					r.System += part.Text
				}
				continue
			}
			if role != "user" && role != "assistant" {
				return fmt.Errorf("unsupported Responses message role")
			}
			r.appendParts(role, content...)
		case "function_call", "custom_tool_call", "tool_search_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			namespace, _ := item["namespace"].(string)
			if kind == "tool_search_call" {
				name, namespace = "tool_search", ""
			}
			if id == "" || name == "" {
				return fmt.Errorf("tool call requires call_id and name")
			}
			arguments := json.RawMessage(`{}`)
			switch {
			case kind == "custom_tool_call":
				arguments, _ = json.Marshal(map[string]any{"input": responsesTextValue(item["input"])})
			case kind == "tool_search_call":
				if args, ok := item["arguments"].(map[string]any); ok {
					arguments, _ = json.Marshal(args)
				}
			default:
				if args, _ := item["arguments"].(string); strings.TrimSpace(args) != "" {
					var object map[string]any
					if decodeProtocolJSON([]byte(args), &object) != nil || object == nil {
						return fmt.Errorf("function arguments must be a JSON object")
					}
					arguments = json.RawMessage(args)
				}
			}
			if unavailable[id] {
				r.appendParts("assistant", textPart(fmt.Sprintf("Historical tool call %s (%s), unavailable in the current tool list:\n%s", name, id, arguments)))
				continue
			}
			r.appendParts("assistant", conversationPart{Kind: partToolCall, CallID: id, Name: toolWireName(namespace, name), Arguments: arguments})
		case "tool_search_output":
			id, _ := item["call_id"].(string)
			if id == "" {
				return fmt.Errorf("tool search output requires call_id")
			}
			found, err := discoveredResponsesTools([]map[string]any{item})
			if err != nil {
				return err
			}
			definitions, _, err := flattenResponsesTools(nil, found)
			if err != nil {
				return err
			}
			listed := make([]map[string]any, 0, len(definitions))
			for _, definition := range definitions {
				listed = append(listed, map[string]any{"name": definition.Name, "description": definition.Description, "parameters": definition.Parameters})
			}
			text := responsesTextValue(listed)
			if unavailable[id] {
				r.appendParts("user", textPart(fmt.Sprintf("Historical tool result (%s):\n%s", id, text)))
			} else {
				r.appendParts("user", conversationPart{Kind: partToolResult, CallID: id, Output: []conversationPart{textPart(text)}})
			}
		case "function_call_output", "custom_tool_call_output":
			id, _ := item["call_id"].(string)
			var output []conversationPart
			if parts, ok := item["output"].([]any); ok {
				converted, err := responsesContentParts(parts)
				if err != nil {
					return err
				}
				output = converted
			} else if text := responsesTextValue(item["output"]); text != "" {
				output = []conversationPart{textPart(text)}
			}
			if id == "" || unavailable[id] {
				label := fmt.Sprintf("Historical tool result (%s):", id)
				if id == "" {
					// Desktop-delivered messages carry a named output without a
					// preceding model call. Do not invent a call for them.
					name, _ := item["name"].(string)
					if name == "" {
						return fmt.Errorf("tool output requires call_id or a notification name")
					}
					if namespace, _ := item["namespace"].(string); namespace != "" {
						name = namespace + "." + name
					}
					label = "Tool notification from " + name + ":"
				}
				r.appendParts("user", append([]conversationPart{textPart(label)}, output...)...)
				continue
			}
			r.appendParts("user", conversationPart{Kind: partToolResult, CallID: id, Output: output})
		case "reasoning":
			id, _ := item["id"].(string)
			if !strings.HasPrefix(id, cclReasoningIDPrefix) {
				// Native reasoning belongs to the provider that issued it. Keep the
				// messages and tool history when a conversation changes backend.
				continue
			}
			var summary strings.Builder
			if parts, ok := item["summary"].([]any); ok {
				for _, part := range parts {
					if p, ok := part.(map[string]any); ok {
						summary.WriteString(responsesTextValue(p["text"]))
					}
				}
			}
			signature, _ := item["encrypted_content"].(string)
			if summary.Len() == 0 && signature == "" {
				continue
			}
			r.appendParts("assistant", conversationPart{Kind: partReasoning, Text: summary.String(), Signature: signature})
		case "compaction":
			encoded, _ := item["encrypted_content"].(string)
			summary, ok := DecodeCompactionSummary(encoded)
			if !ok {
				return fmt.Errorf("opaque native compaction cannot be replayed to this provider")
			}
			r.appendParts("user", textPart("Conversation summary:\n"+summary))
		case "item_reference":
			return fmt.Errorf("item_reference input requires stored responses, which this CCL provider does not keep")
		default:
			if hostedResponsesTool(strings.TrimSuffix(kind, "_call")) {
				// A hosted tool's own record has no meaning to another backend.
				continue
			}
			return fmt.Errorf("unsupported Responses input item %q", kind)
		}
	}
	return nil
}

// effortBudget maps a reasoning effort to a thinking-token budget for upstreams
// that size reasoning in tokens rather than tiers.
func effortBudget(effort string) int {
	switch effort {
	case "minimal", "low":
		return 1024
	case "medium":
		return 2048
	case "high":
		return 4096
	case "xhigh":
		return 8192
	case "max":
		return 16384
	case "ultra":
		return 32768
	default:
		return 0
	}
}
