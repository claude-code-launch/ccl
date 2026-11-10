package oauthproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiFunctionNameSanitizer applies the Gemini function-name rules: Gemini
// function names may only contain [a-zA-Z0-9_.:-] and must start with a letter or
// underscore, capped at 64 bytes. Claude tool names are arbitrary, so they are
// rewritten here and restored from toolNameMap on the response path.
var geminiFunctionNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// geminiToolIDSanitizer produces a safe identifier for the
// synthesized Claude-facing tool_use.id on the response path.
var geminiToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// geminiValidatedMaxTools caps automatic VALIDATED function-calling mode to
// compact tool catalogs (such as Codex's exec/wait/tool_search set). Large
// Claude Code / MCP tool sets stay on AUTO unless the client explicitly sets
// tool_choice, avoiding Antigravity grammar-compilation hangs.
const geminiValidatedMaxTools = 8

// geminiConvertedRequest is the client-facing state plus the Gemini-format
// request body, ready to be wrapped into the Antigravity envelope at call time
// (the project id is only known after credential resolution).
type geminiConvertedRequest struct {
	adapterRequest
	model      string
	effort     string
	geminiBody []byte
}

// sanitizeFunctionName rewrites an arbitrary Claude tool name into a Gemini-safe
// function name.
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
// id ("get_weather-call123" -> "get_weather"), preserving the encoded
func toolNameFromClaudeToolUseID(id string) string {
	parts := strings.Split(id, "-")
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

// sanitizeClaudeToolID produces a Gemini-safe Claude tool identifier.
func sanitizeClaudeToolID(id string) string {
	s := geminiToolIDSanitizer.ReplaceAllString(id, "_")
	if s == "" {
		s = "toolu_" + strings.ReplaceAll(uuidString(), "-", "")
	}
	return s
}

// convertResponsesToGemini converts an OpenAI Responses request body into a
// Gemini-format body (contents/systemInstruction/tools/toolConfig/generationConfig)
// and records the client-facing state the response assembler needs. The Antigravity
// envelope (project/model/requestId/sessionId) is applied separately at call time.
func convertResponsesToGemini(raw []byte) (*geminiConvertedRequest, error) {
	return convertResponsesToGeminiWithSchema(raw, "", sanitizeGeminiSchema)
}

func convertResponsesToAntigravity(raw []byte) (*geminiConvertedRequest, error) {
	return convertResponsesToAntigravityWithCatalog(raw, nil)
}

func convertResponsesToAntigravityWithCatalog(raw []byte, catalog *geminiCatalog) (*geminiConvertedRequest, error) {
	requested := gjson.GetBytes(raw, "model").String()
	canonical := requested
	if catalog != nil {
		canonical = catalog.canonicalModel(requested)
	}
	sanitize := sanitizeGeminiSchema
	if strings.Contains(strings.ToLower(requested), "claude") || strings.Contains(strings.ToLower(canonical), "claude") {
		sanitize = sanitizeAntigravityClaudeSchema
	}
	return convertResponsesToGeminiWithSchema(raw, canonical, sanitize)
}

func convertResponsesToGeminiWithSchema(raw []byte, canonicalModel string, sanitizeSchema func([]byte) []byte) (*geminiConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	clientModel := request.Model
	upstreamModel := stripContextModelSuffix(clientModel)
	if strings.TrimSpace(canonicalModel) != "" {
		upstreamModel = stripContextModelSuffix(strings.TrimSpace(canonicalModel))
	}

	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 32_000
	}

	toolNames, err := buildGeminiToolNames(request)
	if err != nil {
		return nil, err
	}
	out := []byte(`{"contents":[]}`)
	out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", maxTokens)
	if len(request.Stop) > 0 {
		out, _ = sjson.SetBytes(out, "generationConfig.stopSequences", request.Stop)
	}

	if request.System != "" {
		out, _ = sjson.SetBytes(out, "systemInstruction.parts.-1.text", request.System)
	}

	// turns -> contents.
	requireCallID := strings.Contains(strings.ToLower(clientModel), "claude") || strings.Contains(strings.ToLower(upstreamModel), "claude")
	var turns []geminiTurn
	var replayErr error
	var unsignedThinking strings.Builder
	for msgIdx, turn := range request.Turns {
		role := turn.Role
		if role == "" {
			continue
		}
		if role != "assistant" {
			unsignedThinking.Reset()
		}
		geminiRole := role
		if role == "assistant" {
			geminiRole = "model"
		}

		var partItems []string
		for blockIdx, block := range turn.Parts {
			pos := geminiBlockPos{msgIdx: msgIdx, blockIdx: blockIdx}
			if role == "assistant" && toolNames.skipCompanions[pos] {
				continue
			}
			if role == "assistant" && block.Kind == partReasoning {
				if encoded, ok := strings.CutPrefix(block.Signature, geminiPartSignaturePrefix); ok {
					rawPart, err := base64.StdEncoding.DecodeString(encoded)
					part := gjson.ParseBytes(rawPart)
					if err != nil || !gjson.ValidBytes(rawPart) || !part.IsObject() || !part.Get("thoughtSignature").Exists() {
						replayErr = fmt.Errorf("invalid CCL Gemini part signature")
						break
					}
					fallbackThinking := block.Text
					if fallbackThinking == "" {
						fallbackThinking = unsignedThinking.String()
					}
					unsignedThinking.Reset()
					if part.Get("functionCall").Exists() {
						if requireCallID {
							if part.Get("functionCall.id").String() == "" {
								callID := toolNames.signedCallIDs[pos]
								if callID == "" {
									callID = fmt.Sprintf("toolu_gemini_%d_%d", msgIdx, blockIdx)
								}
								rawPart, _ = sjson.SetBytes(rawPart, "functionCall.id", callID)
							}
							rawPart, _ = sjson.DeleteBytes(rawPart, "thoughtSignature")
						}
						partItems = append(partItems, string(rawPart))
						continue
					}
					if part.Get("text").String() == "" && fallbackThinking != "" {
						rawPart, _ = sjson.SetBytes(rawPart, "thought", true)
						rawPart, _ = sjson.SetBytes(rawPart, "text", fallbackThinking)
						part = gjson.ParseBytes(rawPart)
					}
					if requireCallID {
						if part.Get("text").String() == "" {
							continue
						}
						if !part.Get("thought").Bool() {
							rawPart, _ = sjson.DeleteBytes(rawPart, "thoughtSignature")
						}
					} else if geminiRequiresThoughtSignature(upstreamModel) {
						if part.Get("thought").Bool() || strings.TrimSpace(part.Get("text").String()) == "" {
							continue
						}
					}
					partItems = append(partItems, string(rawPart))
					continue
				}
				if block.Text != "" {
					unsignedThinking.WriteString(block.Text)
				}
				continue
			}
			unsignedThinking.Reset()
			switch block.Kind {
			case partText:
				if block.Text == "" {
					continue
				}
				part := []byte(`{"text":""}`)
				part, _ = sjson.SetBytes(part, "text", block.Text)
				partItems = append(partItems, string(part))
			case partToolCall:
				name := block.Name
				if name == "" {
					name = toolNameFromClaudeToolUseID(block.CallID)
				}
				name = toolNames.byOriginal[name]
				if name == "" {
					continue
				}
				args := "{}"
				if len(block.Arguments) > 0 && gjson.ValidBytes(block.Arguments) && gjson.ParseBytes(block.Arguments).IsObject() {
					args = string(block.Arguments)
				}
				part := []byte(`{"functionCall":{"name":"","args":{}}}`)
				if block.CallID != "" {
					part, _ = sjson.SetBytes(part, "functionCall.id", block.CallID)
				}
				part, _ = sjson.SetBytes(part, "functionCall.name", name)
				part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(args))
				partItems = append(partItems, string(part))
			case partToolResult:
				parts := geminiToolResultParts(block, toolNames, requireCallID)
				partItems = append(partItems, parts...)
			case partImage:
				if part, ok := geminiImagePart(block); ok {
					partItems = append(partItems, part)
				}
			}
		}
		if replayErr != nil {
			break
		}
		if len(partItems) > 0 {
			turns = append(turns, geminiTurn{role: geminiRole, parts: partItems})
		}
	}
	if replayErr != nil {
		return nil, replayErr
	}

	contentItems := normalizeGeminiContents(turns, upstreamModel, requireCallID)
	out, _ = sjson.SetRawBytes(out, "contents", []byte(joinRawJSONStrings(contentItems)))

	// tools -> functionDeclarations with a Gemini-safe parameters schema.
	autoToolMode := ""
	if len(request.Tools) > 0 {
		var toolItems []string
		for _, tool := range request.Tools {
			if len(tool.Parameters) == 0 || !gjson.ParseBytes(tool.Parameters).IsObject() {
				continue
			}
			originalName := strings.TrimSpace(tool.Name)
			name := toolNames.byOriginal[originalName]
			if name == "" {
				continue
			}
			declaration := []byte(`{"name":""}`)
			declaration, _ = sjson.SetBytes(declaration, "name", name)
			if description := strings.TrimSpace(tool.Description); description != "" {
				declaration, _ = sjson.SetBytes(declaration, "description", description)
			}
			// Both paths use parameters; Antigravity's Claude route additionally
			// adapts constraints that cannot survive its input_schema bridge.
			declaration, _ = sjson.SetRawBytes(declaration, "parameters", sanitizeSchema(tool.Parameters))
			toolItems = append(toolItems, string(declaration))
		}
		if len(toolItems) > 0 {
			tools := []byte(`[{"functionDeclarations":[]}]`)
			tools, _ = sjson.SetRawBytes(tools, "0.functionDeclarations", []byte(joinRawJSONStrings(toolItems)))
			out, _ = sjson.SetRawBytes(out, "tools", tools)
			// Gemini 3/4 can otherwise emit malformed calls, or names taken from
			// executor documentation rather than the actual declarations. Unlike
			// ANY, VALIDATED still allows ordinary text answers. Keep Claude's
			// Antigravity route and explicit client choices on their own modes,
			// and avoid auto-enabling VALIDATED on large tool catalogs where
			// Antigravity's grammar compiler can stall before streaming headers.
			model := strings.ToLower(upstreamModel)
			if len(toolItems) <= geminiValidatedMaxTools && (strings.HasPrefix(model, "gemini-3") || strings.HasPrefix(model, "gemini-4")) {
				autoToolMode = "VALIDATED"
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", autoToolMode)
			}
		}
	}

	// tool_choice -> toolConfig.functionCallingConfig.
	switch request.ToolChoice.Mode {
	case toolChoiceAuto:
		if autoToolMode != "" {
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", autoToolMode)
		}
	case toolChoiceNone:
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
	case toolChoiceRequired:
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
	case toolChoiceTool:
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
		if request.ToolChoice.Name != "" {
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", []string{toolNames.byOriginal[request.ToolChoice.Name]})
		}
	}

	// reasoning.effort -> generationConfig.thinkingConfig.
	if request.thinkingEnabled() {
		out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", geminiThinkingLevel(request.Effort))
		out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.includeThoughts", true)
	}
	if request.Temperature != nil {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", *request.Temperature)
	}
	if request.TopP != nil {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", *request.TopP)
	}

	adapter := request.adapter(upstreamModel)
	adapter.maxTokens = maxTokens
	adapter.toolNameMap = toolNames.byWire
	return &geminiConvertedRequest{
		adapterRequest: adapter,
		model:          upstreamModel,
		effort:         request.Effort,
		geminiBody:     out,
	}, nil
}

// geminiBypassThoughtSignature is the dummy Gemini documents for a function
// call that has no real thought signature. Codex and other clients replay tool
// history without the opaque part, and Gemini 3/4 reject that turn.
const geminiBypassThoughtSignature = "skip_thought_signature_validator"

// geminiRequiresThoughtSignature reports whether this model rejects a function
// call that does not carry a thought signature.
func geminiRequiresThoughtSignature(model string) bool {
	model = strings.ToLower(stripContextModelSuffix(model))
	return strings.Contains(model, "gemini-3") || strings.Contains(model, "gemini-4")
}

// signUnsignedGeminiFunctionCall sets the documented bypass on every function
// call in one model step that has no signature. Antigravity rejects a later
// unsigned call even when an earlier call in that step is already signed; the
// upstream error names that part's position. A real signature is kept.
func signUnsignedGeminiFunctionCall(model string, parts []string) []string {
	if !geminiRequiresThoughtSignature(model) {
		return parts
	}
	for i, raw := range parts {
		part := gjson.Parse(raw)
		if !part.Get("functionCall").Exists() {
			continue
		}
		if part.Get("thoughtSignature").String() != "" {
			continue
		}
		signed, err := sjson.Set(raw, "thoughtSignature", geminiBypassThoughtSignature)
		if err != nil {
			continue
		}
		parts[i] = signed
	}
	return parts
}

// geminiThinkingLevel maps a client effort onto Gemini's thinkingLevel.
// minimal, low, medium and high pass through. GPT rungs above high
// (xhigh, max, ultra) share high so the request is not rejected.
func geminiThinkingLevel(effort string) string {
	switch effort = strings.ToLower(strings.TrimSpace(effort)); effort {
	case "minimal", "low", "medium", "high":
		return effort
	default:
		return "high"
	}
}

type geminiTurn struct {
	role       string
	parts      []string
	fromSystem bool
}

func geminiPartsHaveField(parts []string, field string) bool {
	for _, raw := range parts {
		if gjson.Get(raw, field).Exists() {
			return true
		}
	}
	return false
}

func appendDeduplicatedGeminiParts(existing, next []string) []string {
	out := append([]string(nil), existing...)
	for _, part := range next {
		if gjson.Get(part, "text").Exists() && !gjson.Get(part, "thought").Bool() && !gjson.Get(part, "thoughtSignature").Exists() {
			duplicate := false
			for _, prev := range out {
				if prev == part {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		}
		out = append(out, part)
	}
	return out
}

func mergeGeminiTurns(turns []geminiTurn, requireCallID bool) []geminiTurn {
	var merged []geminiTurn
	for _, turn := range turns {
		if len(turn.parts) == 0 {
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].role == turn.role {
			prev := &merged[len(merged)-1]
			if turn.role == "model" || requireCallID || geminiPartsHaveField(turn.parts, "functionResponse") || (!prev.fromSystem && !turn.fromSystem) {
				prev.parts = appendDeduplicatedGeminiParts(prev.parts, turn.parts)
				prev.fromSystem = prev.fromSystem && turn.fromSystem
				continue
			}
		}
		merged = append(merged, geminiTurn{
			role:       turn.role,
			parts:      append([]string(nil), turn.parts...),
			fromSystem: turn.fromSystem,
		})
	}
	return merged
}

func geminiIsThinkingOnlyTurn(parts []string) bool {
	if len(parts) == 0 {
		return true
	}
	for _, raw := range parts {
		part := gjson.Parse(raw)
		if part.Get("functionCall").Exists() || part.Get("inline_data").Exists() {
			return false
		}
		if !part.Get("thought").Bool() && strings.TrimSpace(part.Get("text").String()) != "" {
			return false
		}
	}
	return true
}

func normalizeGeminiContents(turns []geminiTurn, upstreamModel string, requireCallID bool) []string {
	merged := mergeGeminiTurns(turns, requireCallID)
	// Strip model turns that contain only thought or empty text parts (from an
	// interrupted or empty response) and coalesce the surrounding user turns so
	// the model does not imitate empty turns on follow-up prompts.
	var cleaned []geminiTurn
	for _, turn := range merged {
		if turn.role == "model" && geminiIsThinkingOnlyTurn(turn.parts) {
			continue
		}
		cleaned = append(cleaned, turn)
	}
	merged = mergeGeminiTurns(cleaned, requireCallID)
	for i := range merged {
		if merged[i].role == "model" {
			merged[i].parts = signUnsignedGeminiFunctionCall(upstreamModel, merged[i].parts)
		}
	}
	// Strip a trailing model turn with unanswered function calls: the model is
	// about to answer them, so the dangling calls must not be replayed upstream.
	if len(merged) > 0 && merged[len(merged)-1].role == "model" && geminiPartsHaveField(merged[len(merged)-1].parts, "functionCall") {
		merged = merged[:len(merged)-1]
	}
	if requireCallID || geminiRequiresThoughtSignature(upstreamModel) {
		for i := range merged {
			if merged[i].role != "model" {
				continue
			}
			var thoughts, others, calls []string
			seenCallIDs := map[string]bool{}
			for _, raw := range merged[i].parts {
				part := gjson.Parse(raw)
				switch {
				case part.Get("thought").Bool():
					thoughts = append(thoughts, raw)
				case part.Get("functionCall").Exists():
					id := part.Get("functionCall.id").String()
					if id != "" {
						if seenCallIDs[id] {
							continue
						}
						seenCallIDs[id] = true
					}
					calls = append(calls, raw)
				default:
					others = append(others, raw)
				}
			}
			merged[i].parts = append(append(thoughts, others...), calls...)
		}

		var paired []geminiTurn
		for i := 0; i < len(merged); i++ {
			turn := merged[i]
			if turn.role == "user" {
				var others []string
				for _, raw := range turn.parts {
					if !gjson.Get(raw, "functionResponse").Exists() {
						others = append(others, raw)
					}
				}
				turn.parts = others
				if len(turn.parts) > 0 {
					paired = append(paired, turn)
				}
				continue
			}
			paired = append(paired, turn)
			var calls []struct{ id, name string }
			for _, raw := range turn.parts {
				call := gjson.Get(raw, "functionCall")
				if call.Exists() {
					calls = append(calls, struct{ id, name string }{
						id:   call.Get("id").String(),
						name: call.Get("name").String(),
					})
				}
			}
			if len(calls) == 0 || i == len(merged)-1 {
				continue
			}
			var nextUser geminiTurn
			if i+1 < len(merged) && merged[i+1].role == "user" {
				nextUser = merged[i+1]
				i++
			} else {
				nextUser = geminiTurn{role: "user"}
			}
			byID := map[string]string{}
			var unkeyed, others []string
			for _, raw := range nextUser.parts {
				fr := gjson.Get(raw, "functionResponse")
				if !fr.Exists() {
					others = append(others, raw)
					continue
				}
				id := fr.Get("id").String()
				if id == "" {
					unkeyed = append(unkeyed, raw)
				} else if _, exists := byID[id]; !exists {
					byID[id] = raw
				}
			}
			var orderedResponses []string
			for _, call := range calls {
				if call.id != "" {
					if raw, ok := byID[call.id]; ok {
						orderedResponses = append(orderedResponses, raw)
						delete(byID, call.id)
						continue
					}
				} else if len(unkeyed) > 0 {
					orderedResponses = append(orderedResponses, unkeyed[0])
					unkeyed = unkeyed[1:]
					continue
				}
				fr := []byte(`{"functionResponse":{"name":"","response":{"result":"[Tool execution was interrupted or skipped]"}}}`)
				if call.id != "" {
					fr, _ = sjson.SetBytes(fr, "functionResponse.id", call.id)
				}
				fr, _ = sjson.SetBytes(fr, "functionResponse.name", call.name)
				orderedResponses = append(orderedResponses, string(fr))
			}
			nextUser.parts = append(orderedResponses, others...)
			nextUser.fromSystem = false
			if len(nextUser.parts) > 0 {
				paired = append(paired, nextUser)
			}
		}
		merged = mergeGeminiTurns(paired, requireCallID)
		if len(merged) > 0 && merged[len(merged)-1].role == "model" && geminiPartsHaveField(merged[len(merged)-1].parts, "functionCall") {
			merged = merged[:len(merged)-1]
		}
	}
	contentItems := make([]string, 0, len(merged))
	for _, turn := range merged {
		if len(turn.parts) > 0 {
			contentItems = append(contentItems, geminiContentWithParts(turn.role, turn.parts))
		}
	}
	return contentItems
}

// geminiContentWithParts builds one Gemini content object from a role and its raw
// part JSON values.
func geminiContentWithParts(role string, parts []string) string {
	content := []byte(`{"role":"","parts":[]}`)
	content, _ = sjson.SetBytes(content, "role", role)
	content, _ = sjson.SetRawBytes(content, "parts", []byte(joinRawJSONStrings(parts)))
	return string(content)
}

// geminiImagePart converts an inline base64 image part into a Gemini
// inline_data part.
func geminiImagePart(block conversationPart) (string, bool) {
	if block.MediaType == "" || block.Data == "" {
		return "", false
	}
	part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inline_data.mime_type", block.MediaType)
	part, _ = sjson.SetBytes(part, "inline_data.data", block.Data)
	return string(part), true
}

// geminiToolResultParts converts a tool result part into Gemini
// functionResponse/inline_data parts using the same name map as declarations.
func geminiToolResultParts(block conversationPart, names *geminiToolNames, requireCallID bool) []string {
	toolUseID := block.CallID
	if toolUseID == "" {
		return nil
	}
	funcName := names.byOriginal[names.resultName(toolUseID)]

	images := block.outputImages()
	parts := make([]string, 0, 1+len(images))
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	if id := names.resultIDForModel(toolUseID, requireCallID); id != "" {
		fr, _ = sjson.SetBytes(fr, "functionResponse.id", id)
	}
	fr, _ = sjson.SetBytes(fr, "functionResponse.name", funcName)
	fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", block.outputText())
	parts = append(parts, string(fr))
	for _, image := range images {
		if image.MediaType == "" || image.Data == "" {
			continue
		}
		part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inline_data.mime_type", image.MediaType)
		part, _ = sjson.SetBytes(part, "inline_data.data", image.Data)
		parts = append(parts, string(part))
	}
	return parts
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
