package oauthproxy

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// CommandCode's generate API always streams NDJSON and wraps the conversation
// in an envelope that mimics the official CLI's session context. These bounds
// mirror the reference proxy implementation.
const (
	commandcodeMaxTokensCap    = 200000
	commandcodeDefaultMaxToken = 64000
	// commandcodeNodeVersion fills the environment string the CLI reports.
	// CommandCode only parses it loosely, so a current LTS value is enough.
	commandcodeNodeVersion = "22.21.1"
)

// commandcodeConvertedRequest carries the CommandCode /alpha/generate body
// plus the adapter metadata the shared Anthropic assembler needs.
type commandcodeConvertedRequest struct {
	anthropicAdapterRequest
	body map[string]any
}

// convertAnthropicToCommandCode builds a CommandCode generate request from an
// Anthropic Messages body. Text, image, tool_use and tool_result blocks map
// onto CommandCode's part shapes; thinking blocks are dropped because the CLI
// protocol has no replayable thinking part.
func convertAnthropicToCommandCode(raw []byte) (*commandcodeConvertedRequest, error) {
	root := gjson.ParseBytes(raw)
	model := stripContextModelSuffix(strings.TrimSpace(root.Get("model").String()))
	if model == "" {
		return nil, fmt.Errorf("commandcode request has no model")
	}

	// tool_use id → name lookup so tool_result parts can carry the tool name
	// CommandCode expects even though Anthropic tool_result blocks omit it.
	toolNames := make(map[string]string)
	for _, message := range root.Get("messages").Array() {
		if message.Get("role").String() != "assistant" {
			continue
		}
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() != "tool_use" {
				continue
			}
			id := strings.TrimSpace(block.Get("id").String())
			name := strings.TrimSpace(block.Get("name").String())
			if id != "" && name != "" {
				toolNames[id] = name
			}
		}
	}

	ccMessages := make([]map[string]any, 0, 8)
	for _, message := range root.Get("messages").Array() {
		role := message.Get("role").String()
		content := message.Get("content")
		switch role {
		case "user", "assistant":
		default:
			continue
		}
		parts := make([]map[string]any, 0, 4)
		if content.IsArray() {
			for _, block := range content.Array() {
				part, ok := commandcodePartFromAnthropic(block, toolNames)
				if ok {
					parts = append(parts, part)
				}
			}
		} else if text := strings.TrimSpace(content.String()); text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": content.String()})
		}
		if len(parts) == 0 {
			continue
		}
		ccMessages = append(ccMessages, map[string]any{"role": role, "content": parts})
	}

	maxTokens := int(root.Get("max_tokens").Int())
	if maxTokens <= 0 {
		maxTokens = commandcodeDefaultMaxToken
	}
	if maxTokens > commandcodeMaxTokensCap {
		maxTokens = commandcodeMaxTokensCap
	}

	params := map[string]any{
		"model":      model,
		"messages":   ccMessages,
		"max_tokens": maxTokens,
		// CommandCode always streams; non-streaming clients are served by
		// accumulating the NDJSON stream.
		"stream": true,
	}
	if system := commandcodeSystemPrompt(root.Get("system")); system != "" {
		params["system"] = system
	}
	if root.Get("temperature").Exists() {
		params["temperature"] = root.Get("temperature").Float()
	}
	if tools := commandcodeTools(root.Get("tools")); len(tools) > 0 {
		params["tools"] = tools
	}
	if choice := commandcodeToolChoice(root.Get("tool_choice")); len(choice) > 0 {
		params["tool_choice"] = choice
	}

	body := map[string]any{
		"config": map[string]any{
			"workingDir":    "/",
			"date":          time.Now().UTC().Format("2006-01-02"),
			"environment":   fmt.Sprintf("%s-%s, Node.js %s", runtime.GOOS, runtime.GOARCH, commandcodeNodeVersion),
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         "",
		"permissionMode": "standard",
		"params":         params,
	}

	return &commandcodeConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel: model,
			clientModel:   root.Get("model").String(),
			stream:        root.Get("stream").Bool(),
			maxTokens:     maxTokens,
			inputTokens:   estimateApproxTokensBytes(raw),
			// CommandCode accepts Anthropic tool names verbatim, so there is no
			// name-shortening to restore on the response path.
			toolNameMap: map[string]string{},
		},
		body: body,
	}, nil
}

// commandcodePartFromAnthropic maps one Anthropic content block onto a
// CommandCode part.
func commandcodePartFromAnthropic(block gjson.Result, toolNames map[string]string) (map[string]any, bool) {
	switch block.Get("type").String() {
	case "text":
		text := block.Get("text").String()
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		return map[string]any{"type": "text", "text": text}, true
	case "image":
		image := commandcodeImageURL(block.Get("source"))
		if image == "" {
			return nil, false
		}
		return map[string]any{"type": "image", "image": image}, true
	case "tool_use":
		name := strings.TrimSpace(block.Get("name").String())
		if name == "" {
			return nil, false
		}
		input := block.Get("input").Raw
		if strings.TrimSpace(input) == "" || !gjson.Valid(input) {
			input = "{}"
		}
		return map[string]any{
			"type":       "tool-call",
			"toolCallId": block.Get("id").String(),
			"toolName":   name,
			"input":      gjson.Parse(input).Value(),
		}, true
	case "tool_result":
		id := strings.TrimSpace(block.Get("tool_use_id").String())
		name := toolNames[id]
		value := commandcodeToolResultOutput(block.Get("content"))
		return map[string]any{
			"type":       "tool-result",
			"toolCallId": id,
			"toolName":   name,
			"output":     map[string]any{"type": "text", "value": value},
		}, true
	case "thinking":
		// The CLI protocol has no replayable thinking part.
		return nil, false
	default:
		return nil, false
	}
}

// commandcodeImageURL converts an Anthropic image source into the data-URL (or
// remote URL) form CommandCode expects.
func commandcodeImageURL(source gjson.Result) string {
	switch source.Get("type").String() {
	case "base64":
		mediaType := strings.TrimSpace(source.Get("media_type").String())
		if mediaType == "" {
			mediaType = "image/png"
		}
		data := strings.TrimSpace(source.Get("data").String())
		if data == "" {
			return ""
		}
		return "data:" + mediaType + ";base64," + data
	case "url":
		return strings.TrimSpace(source.Get("url").String())
	default:
		return ""
	}
}

// commandcodeToolResultOutput flattens tool_result content (string or block
// array) into the single text value CommandCode carries.
func commandcodeToolResultOutput(content gjson.Result) string {
	if content.IsArray() {
		var fragments []string
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				fragments = append(fragments, block.Get("text").String())
			}
		}
		return strings.Join(fragments, "\n")
	}
	return content.String()
}

// commandcodeSystemPrompt flattens the Anthropic system field (string or text
// block array).
func commandcodeSystemPrompt(system gjson.Result) string {
	if !system.Exists() {
		return ""
	}
	if system.IsArray() {
		var fragments []string
		for _, block := range system.Array() {
			if text := strings.TrimSpace(block.Get("text").String()); text != "" {
				fragments = append(fragments, text)
			}
		}
		return strings.Join(fragments, "\n")
	}
	return strings.TrimSpace(system.String())
}

// commandcodeTools reshapes Anthropic tool definitions; CommandCode uses the
// same name/description/input_schema triple.
func commandcodeTools(tools gjson.Result) []any {
	if !tools.IsArray() {
		return nil
	}
	converted := make([]any, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			continue
		}
		schema := tool.Get("input_schema").Raw
		if strings.TrimSpace(schema) == "" || !gjson.Valid(schema) {
			schema = `{"type":"object","properties":{}}`
		}
		converted = append(converted, map[string]any{
			"type":         "function",
			"name":         name,
			"description":  tool.Get("description").String(),
			"input_schema": gjson.Parse(schema).Value(),
		})
	}
	return converted
}

// commandcodeToolChoice passes the Anthropic choice through; the vocabularies
// (auto/any/tool/none) already match.
func commandcodeToolChoice(choice gjson.Result) map[string]any {
	if !choice.IsObject() {
		return nil
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	if choiceType == "" {
		return nil
	}
	converted := map[string]any{"type": choiceType}
	if name := strings.TrimSpace(choice.Get("name").String()); name != "" {
		converted["name"] = name
	}
	return converted
}
