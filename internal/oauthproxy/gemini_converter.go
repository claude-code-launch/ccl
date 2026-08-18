package oauthproxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiFunctionNameSanitizer mirrors CPA's functionNameSanitizer: Gemini
// function names may only contain [a-zA-Z0-9_.:-] and must start with a letter or
// underscore, capped at 64 bytes. Claude tool names are arbitrary, so they are
// rewritten here and restored from toolNameMap on the response path.
var geminiFunctionNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// geminiToolIDSanitizer mirrors CPA's claudeToolUseIDSanitizer for the
// synthesized Claude-facing tool_use.id on the response path.
var geminiToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// geminiConvertedRequest is the client-facing state plus the Gemini-format
// request body, ready to be wrapped into the Antigravity envelope at call time
// (the project id is only known after credential resolution).
type geminiConvertedRequest struct {
	anthropicAdapterRequest
	model      string
	geminiBody []byte
}

// sanitizeFunctionName rewrites an arbitrary Claude tool name into a Gemini-safe
// function name, mirroring CPA's util.SanitizeFunctionName.
func sanitizeFunctionName(name string) string {
	if name == "" {
		return ""
	}
	sanitized := geminiFunctionNameSanitizer.ReplaceAllString(name, "_")
	if len(sanitized) > 0 {
		first := sanitized[0]
		if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
			if len(sanitized) >= 64 {
				sanitized = sanitized[:63]
			}
			sanitized = "_" + sanitized
		}
	} else {
		sanitized = "_"
	}
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

// toolNameFromClaudeToolUseID recovers the tool name encoded in a Claude tool_use
// id ("get_weather-call123" -> "get_weather"), mirroring CPA's
// toolNameFromClaudeToolUseID.
func toolNameFromClaudeToolUseID(id string) string {
	parts := strings.Split(id, "-")
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

// sanitizeClaudeToolID mirrors CPA's util.SanitizeClaudeToolID.
func sanitizeClaudeToolID(id string) string {
	s := geminiToolIDSanitizer.ReplaceAllString(id, "_")
	if s == "" {
		s = "toolu_" + strings.ReplaceAll(uuidString(), "-", "")
	}
	return s
}

// isClaudeCodeAttributionSystemText reports whether a system block is the Claude
// Code billing/fingerprint attribution that must not be forwarded upstream.
func isClaudeCodeAttributionSystemText(text string) bool {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	return strings.HasPrefix(text, "x-anthropic-billing-header:")
}

// convertAnthropicToGemini converts an Anthropic Messages request body into a
// Gemini-format body (contents/systemInstruction/tools/toolConfig/generationConfig)
// and records the client-facing state the response assembler needs. The Antigravity
// envelope (project/model/requestId/sessionId) is applied separately at call time.
func convertAnthropicToGemini(raw []byte) (*geminiConvertedRequest, error) {
	modelResult := gjson.GetBytes(raw, "model")
	if modelResult.Type != gjson.String || strings.TrimSpace(modelResult.String()) == "" {
		return nil, fmt.Errorf("model is required")
	}
	clientModel := strings.TrimSpace(modelResult.String())
	upstreamModel := stripContextModelSuffix(clientModel)

	if messagesResult := gjson.GetBytes(raw, "messages"); !messagesResult.IsArray() || len(messagesResult.Array()) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	stream := gjson.GetBytes(raw, "stream").Bool()
	maxTokens := int(gjson.GetBytes(raw, "max_tokens").Int())
	if maxTokens <= 0 {
		maxTokens = 32_000
	}

	toolNameMap := make(map[string]string)
	out := []byte(`{"contents":[]}`)

	// system instruction (string or array of text blocks).
	systemResult := gjson.GetBytes(raw, "system")
	if systemResult.IsArray() {
		var systemParts []string
		systemResult.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "text" {
				return true
			}
			text := block.Get("text")
			if text.Type != gjson.String || isClaudeCodeAttributionSystemText(text.String()) {
				return true
			}
			part := []byte(`{"text":""}`)
			part, _ = sjson.SetBytes(part, "text", text.String())
			systemParts = append(systemParts, string(part))
			return true
		})
		if len(systemParts) > 0 {
			systemInstruction := []byte(`{"role":"user","parts":[]}`)
			systemInstruction, _ = sjson.SetRawBytes(systemInstruction, "parts", []byte(joinRawJSONStrings(systemParts)))
			out, _ = sjson.SetRawBytes(out, "systemInstruction", systemInstruction)
		}
	} else if systemResult.Type == gjson.String && !isClaudeCodeAttributionSystemText(systemResult.String()) {
		out, _ = sjson.SetBytes(out, "systemInstruction.parts.-1.text", systemResult.String())
	}

	// messages -> contents.
	var contentItems []string
	gjson.GetBytes(raw, "messages").ForEach(func(_, message gjson.Result) bool {
		role := message.Get("role").String()
		if role == "" {
			return true
		}
		geminiRole := role
		if role == "assistant" {
			geminiRole = "model"
		} else if role == "system" {
			geminiRole = "user"
		}

		var partItems []string
		content := message.Get("content")
		if role == "system" {
			// A mid-conversation system message carries a reminder. Forward only a
			// plain-text reminder; skip structured content to keep it simple.
			if content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
				partItems = append(partItems, jsonStringValue(`{"text":`+jsonStringValue(content.String())+`}`))
				contentItems = append(contentItems, geminiContentWithParts(geminiRole, partItems))
			}
			return true
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				switch block.Get("type").String() {
				case "text":
					text := block.Get("text").String()
					if text == "" {
						return true
					}
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", text)
					partItems = append(partItems, string(part))
				case "tool_use":
					name := block.Get("name").String()
					if name == "" {
						name = toolNameFromClaudeToolUseID(block.Get("id").String())
					}
					originalName := name
					name = sanitizeFunctionName(name)
					if name == "" {
						return true
					}
					if originalName != "" {
						toolNameMap[name] = originalName
					}
					args := block.Get("input").String()
					if gjson.Valid(args) && gjson.Parse(args).IsObject() {
						part := []byte(`{"functionCall":{"name":"","args":{}}}`)
						part, _ = sjson.SetBytes(part, "functionCall.name", name)
						part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(args))
						partItems = append(partItems, string(part))
					}
				case "tool_result":
					parts := geminiToolResultParts(block, toolNameMap)
					partItems = append(partItems, parts...)
				case "image":
					if part, ok := geminiImagePart(block); ok {
						partItems = append(partItems, part)
					}
				}
				return true
			})
			if len(partItems) > 0 {
				contentItems = append(contentItems, geminiContentWithParts(geminiRole, partItems))
			}
		} else if content.Type == gjson.String {
			partItems = append(partItems, jsonStringValue(`{"text":`+jsonStringValue(content.String())+`}`))
			contentItems = append(contentItems, geminiContentWithParts(geminiRole, partItems))
		}
		return true
	})

	// Strip a trailing model turn with unanswered function calls: the model is
	// about to answer them, so the dangling calls must not be replayed upstream.
	if len(contentItems) > 0 {
		last := gjson.Parse(contentItems[len(contentItems)-1])
		if last.Get("role").String() == "model" {
			hasFunctionCall := false
			last.Get("parts").ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					hasFunctionCall = true
					return false
				}
				return true
			})
			if hasFunctionCall {
				contentItems = contentItems[:len(contentItems)-1]
			}
		}
	}
	out, _ = sjson.SetRawBytes(out, "contents", []byte(joinRawJSONStrings(contentItems)))

	// tools -> functionDeclarations with a Gemini-safe parameters schema.
	if toolsResult := gjson.GetBytes(raw, "tools"); toolsResult.IsArray() {
		var toolItems []string
		toolsResult.ForEach(func(_, tool gjson.Result) bool {
			schema := tool.Get("input_schema")
			if !schema.Exists() || !schema.IsObject() {
				return true
			}
			originalName := strings.TrimSpace(tool.Get("name").String())
			name := sanitizeFunctionName(originalName)
			if name == "" {
				return true
			}
			if originalName != "" {
				toolNameMap[name] = originalName
			}
			declaration := []byte(`{"name":""}`)
			declaration, _ = sjson.SetBytes(declaration, "name", name)
			if description := strings.TrimSpace(tool.Get("description").String()); description != "" {
				declaration, _ = sjson.SetBytes(declaration, "description", description)
			}
			declaration, _ = sjson.SetRawBytes(declaration, "parametersJsonSchema", []byte(schema.Raw))
			toolItems = append(toolItems, string(declaration))
			return true
		})
		if len(toolItems) > 0 {
			tools := []byte(`[{"functionDeclarations":[]}]`)
			tools, _ = sjson.SetRawBytes(tools, "0.functionDeclarations", []byte(joinRawJSONStrings(toolItems)))
			out, _ = sjson.SetRawBytes(out, "tools", tools)
		}
	}

	// tool_choice -> toolConfig.functionCallingConfig.
	toolChoice := gjson.GetBytes(raw, "tool_choice")
	if toolChoice.Exists() {
		choiceType := ""
		choiceName := ""
		if toolChoice.IsObject() {
			choiceType = toolChoice.Get("type").String()
			choiceName = toolChoice.Get("name").String()
		} else if toolChoice.Type == gjson.String {
			choiceType = toolChoice.String()
		}
		switch choiceType {
		case "auto":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "AUTO")
		case "none":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case "any":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
		case "tool":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
			if choiceName != "" {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", []string{sanitizeFunctionName(choiceName)})
			}
		}
	}

	// thinking -> generationConfig.thinkingConfig.
	thinkingEnabled := false
	if thinking := gjson.GetBytes(raw, "thinking"); thinking.IsObject() {
		switch thinking.Get("type").String() {
		case "enabled":
			thinkingEnabled = true
			if budget := thinking.Get("budget_tokens"); budget.Type == gjson.Number {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingBudget", budget.Int())
			}
		case "adaptive", "auto":
			thinkingEnabled = true
			if effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(raw, "output_config.effort").String())); effort != "" {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", effort)
			} else {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", "high")
			}
		}
	}
	if temperature := gjson.GetBytes(raw, "temperature"); temperature.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", temperature.Num)
	}
	if topP := gjson.GetBytes(raw, "top_p"); topP.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", topP.Num)
	}
	if topK := gjson.GetBytes(raw, "top_k"); topK.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topK", topK.Num)
	}

	return &geminiConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{
			upstreamModel:     upstreamModel,
			clientModel:       clientModel,
			stream:            stream,
			thinkingEnabled:   thinkingEnabled,
			thinkingSignature: "gemini",
			maxTokens:         maxTokens,
			inputTokens:       estimateApproxTokensBytes(raw),
			toolNameMap:       toolNameMap,
		},
		model:      upstreamModel,
		geminiBody: out,
	}, nil
}

// geminiContentWithParts builds one Gemini content object from a role and its raw
// part JSON values.
func geminiContentWithParts(role string, parts []string) string {
	content := []byte(`{"role":"","parts":[]}`)
	content, _ = sjson.SetBytes(content, "role", role)
	content, _ = sjson.SetRawBytes(content, "parts", []byte(joinRawJSONStrings(parts)))
	return string(content)
}

// geminiImagePart converts an Anthropic base64 image block into a Gemini
// inline_data part.
func geminiImagePart(block gjson.Result) (string, bool) {
	source := block.Get("source")
	if source.Get("type").String() != "base64" {
		return "", false
	}
	mimeType := source.Get("media_type").String()
	data := source.Get("data").String()
	if mimeType == "" || data == "" {
		return "", false
	}
	part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inline_data.mime_type", mimeType)
	part, _ = sjson.SetBytes(part, "inline_data.data", data)
	return string(part), true
}

// geminiToolResultParts converts a Claude tool_result content block into Gemini
// functionResponse/inline_data parts, recording the sanitized->original name
// mapping so the response path can restore the client tool name.
func geminiToolResultParts(block gjson.Result, toolNameMap map[string]string) []string {
	toolUseID := block.Get("tool_use_id").String()
	if toolUseID == "" {
		return nil
	}
	funcName := toolNameFromClaudeToolUseID(toolUseID)
	if funcName == "" {
		funcName = toolUseID
	}
	originalName := funcName
	funcName = sanitizeFunctionName(funcName)
	if originalName != "" {
		toolNameMap[funcName] = originalName
	}

	result, resultIsRaw, images := geminiToolResultContent(block.Get("content"))
	parts := make([]string, 0, 1+len(images))
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	fr, _ = sjson.SetBytes(fr, "functionResponse.name", funcName)
	if resultIsRaw {
		fr, _ = sjson.SetRawBytes(fr, "functionResponse.response.result", []byte(result))
	} else {
		fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", result)
	}
	parts = append(parts, string(fr))
	for _, image := range images {
		part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inline_data.mime_type", image.mimeType)
		part, _ = sjson.SetBytes(part, "inline_data.data", image.data)
		parts = append(parts, string(part))
	}
	return parts
}

// geminiToolResultImage is a base64 image block separated out of a tool result.
type geminiToolResultImage struct {
	mimeType string
	data     string
}

// geminiToolResultContent normalizes a Claude tool_result content field into a
// Gemini functionResponse result (string or raw JSON) plus any separated image
// blocks, mirroring CPA's ConvertClaudeToolResultContent.
func geminiToolResultContent(content gjson.Result) (result string, resultIsRaw bool, images []geminiToolResultImage) {
	switch {
	case content.Type == gjson.String:
		return content.String(), false, nil
	case content.IsArray():
		nonImageCount := 0
		lastNonImage := ""
		filtered := []byte(`[]`)
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "image" && block.Get("source.type").String() == "base64" {
				if image, ok := geminiToolResultImageFromBlock(block); ok {
					images = append(images, image)
				}
				return true
			}
			nonImageCount++
			lastNonImage = block.Raw
			filtered, _ = sjson.SetRawBytes(filtered, "-1", []byte(block.Raw))
			return true
		})
		switch {
		case nonImageCount == 1:
			return lastNonImage, true, images
		case nonImageCount > 1:
			return string(filtered), true, images
		default:
			return "", false, images
		}
	case content.IsObject():
		if content.Get("type").String() == "image" && content.Get("source.type").String() == "base64" {
			if image, ok := geminiToolResultImageFromBlock(content); ok {
				return "", false, []geminiToolResultImage{image}
			}
			return "", false, nil
		}
		return content.Raw, true, nil
	case content.Raw != "":
		return content.Raw, true, nil
	default:
		return "", false, nil
	}
}

func geminiToolResultImageFromBlock(block gjson.Result) (geminiToolResultImage, bool) {
	data := block.Get("source.data").String()
	if data == "" {
		return geminiToolResultImage{}, false
	}
	return geminiToolResultImage{
		mimeType: block.Get("source.media_type").String(),
		data:     data,
	}, true
}

// jsonStringValue marshals s as a JSON string literal.
func jsonStringValue(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// antigravityEnvelope wraps a Gemini body into the Antigravity request envelope:
// project/model/request plus the requestType/requestId/sessionId fields the
// cloudcode-pa control plane expects.
func antigravityEnvelope(geminiBody []byte, modelName, projectID, sessionID string) []byte {
	out := []byte(`{"project":"","request":{},"model":"","userAgent":"antigravity","requestType":"agent"}`)
	out, _ = sjson.SetBytes(out, "model", modelName)
	out, _ = sjson.SetRawBytes(out, "request", geminiBody)
	out, _ = sjson.SetBytes(out, "project", projectID)
	out, _ = sjson.SetBytes(out, "requestId", "agent-"+uuidString())
	out, _ = sjson.SetBytes(out, "request.sessionId", sessionID)
	return out
}
