package oauthproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

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
	anthropicAdapterRequest
	model      string
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
	return convertAnthropicToGeminiWithSchema(raw, "", sanitizeGeminiSchema)
}

func convertAnthropicToAntigravity(raw []byte) (*geminiConvertedRequest, error) {
	return convertAnthropicToAntigravityWithCatalog(raw, nil)
}

func convertAnthropicToAntigravityWithCatalog(raw []byte, catalog *geminiCatalog) (*geminiConvertedRequest, error) {
	requested := gjson.GetBytes(raw, "model").String()
	canonical := requested
	if catalog != nil {
		canonical = catalog.canonicalModel(requested)
	}
	sanitize := sanitizeGeminiSchema
	if strings.Contains(strings.ToLower(requested), "claude") || strings.Contains(strings.ToLower(canonical), "claude") {
		sanitize = sanitizeAntigravityClaudeSchema
	}
	return convertAnthropicToGeminiWithSchema(raw, canonical, sanitize)
}

func convertAnthropicToGeminiWithSchema(raw []byte, canonicalModel string, sanitizeSchema func([]byte) []byte) (*geminiConvertedRequest, error) {
	modelResult := gjson.GetBytes(raw, "model")
	if modelResult.Type != gjson.String || strings.TrimSpace(modelResult.String()) == "" {
		return nil, fmt.Errorf("model is required")
	}
	clientModel := strings.TrimSpace(modelResult.String())
	upstreamModel := stripContextModelSuffix(clientModel)
	if strings.TrimSpace(canonicalModel) != "" {
		upstreamModel = stripContextModelSuffix(strings.TrimSpace(canonicalModel))
	}

	if messagesResult := gjson.GetBytes(raw, "messages"); !messagesResult.IsArray() || len(messagesResult.Array()) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	stream := gjson.GetBytes(raw, "stream").Bool()
	maxTokens := int(gjson.GetBytes(raw, "max_tokens").Int())
	if maxTokens <= 0 {
		maxTokens = 32_000
	}

	toolNames, err := buildGeminiToolNames(raw)
	if err != nil {
		return nil, err
	}
	out := []byte(`{"contents":[]}`)
	out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", maxTokens)
	if stops := gjson.GetBytes(raw, "stop_sequences"); stops.Exists() && stops.Type != gjson.Null {
		var sequences []string
		if err := json.Unmarshal([]byte(stops.Raw), &sequences); err != nil {
			return nil, fmt.Errorf("stop_sequences must be an array of strings: %w", err)
		}
		out, _ = sjson.SetBytes(out, "generationConfig.stopSequences", sequences)
	}

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
	requireCallID := strings.Contains(strings.ToLower(clientModel), "claude") || strings.Contains(strings.ToLower(upstreamModel), "claude")
	var turns []geminiTurn
	var replayErr error
	var unsignedThinking strings.Builder
	for msgIdx, message := range gjson.GetBytes(raw, "messages").Array() {
		role := message.Get("role").String()
		if role == "" {
			continue
		}
		if role != "assistant" {
			unsignedThinking.Reset()
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
			// A system message in the list carries instructions or a reminder, as a
			// string or as text blocks; it is forwarded as user text.
			var texts []string
			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					if block.Get("type").String() == "text" {
						texts = append(texts, block.Get("text").String())
					}
					return true
				})
			} else if content.Type == gjson.String {
				texts = append(texts, content.String())
			}
			for _, text := range texts {
				if strings.TrimSpace(text) != "" {
					partItems = append(partItems, `{"text":`+jsonStringValue(text)+`}`)
				}
			}
			if len(partItems) > 0 {
				turns = append(turns, geminiTurn{role: geminiRole, parts: partItems, fromSystem: true})
			}
			continue
		}
		if content.IsArray() {
			for blockIdx, block := range content.Array() {
				pos := geminiBlockPos{msgIdx: msgIdx, blockIdx: blockIdx}
				kind := block.Get("type").String()
				if role == "assistant" && toolNames.skipCompanions[pos] {
					continue
				}
				if role == "assistant" && kind == "thinking" {
					if encoded, ok := strings.CutPrefix(block.Get("signature").String(), geminiPartSignaturePrefix); ok {
						rawPart, err := base64.StdEncoding.DecodeString(encoded)
						part := gjson.ParseBytes(rawPart)
						if err != nil || !gjson.ValidBytes(rawPart) || !part.IsObject() || !part.Get("thoughtSignature").Exists() {
							replayErr = fmt.Errorf("invalid CCL Gemini part signature")
							break
						}
						fallbackThinking := block.Get("thinking").String()
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
					if thinking := block.Get("thinking").String(); thinking != "" {
						unsignedThinking.WriteString(thinking)
					}
					continue
				}
				unsignedThinking.Reset()
				switch kind {
				case "text":
					text := block.Get("text").String()
					if text == "" {
						continue
					}
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", text)
					partItems = append(partItems, string(part))
				case "tool_use":
					name := block.Get("name").String()
					if name == "" {
						name = toolNameFromClaudeToolUseID(block.Get("id").String())
					}
					name = toolNames.byOriginal[name]
					if name == "" {
						continue
					}
					args := "{}"
					if input := block.Get("input"); input.IsObject() {
						args = input.Raw
					} else if s := input.String(); gjson.Valid(s) && gjson.Parse(s).IsObject() {
						args = s
					}
					part := []byte(`{"functionCall":{"name":"","args":{}}}`)
					if id := block.Get("id").String(); id != "" {
						part, _ = sjson.SetBytes(part, "functionCall.id", id)
					}
					part, _ = sjson.SetBytes(part, "functionCall.name", name)
					part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(args))
					partItems = append(partItems, string(part))
				case "tool_result":
					parts := geminiToolResultParts(block, toolNames, requireCallID)
					partItems = append(partItems, parts...)
				case "image":
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
		} else if content.Type == gjson.String {
			partItems = append(partItems, `{"text":`+jsonStringValue(content.String())+`}`)
			turns = append(turns, geminiTurn{role: geminiRole, parts: partItems})
		}
	}
	if replayErr != nil {
		return nil, replayErr
	}

	contentItems := normalizeGeminiContents(turns, upstreamModel, requireCallID)
	out, _ = sjson.SetRawBytes(out, "contents", []byte(joinRawJSONStrings(contentItems)))

	// tools -> functionDeclarations with a Gemini-safe parameters schema.
	autoToolMode := "AUTO"
	if toolsResult := gjson.GetBytes(raw, "tools"); toolsResult.IsArray() {
		var toolItems []string
		toolsResult.ForEach(func(_, tool gjson.Result) bool {
			schema := tool.Get("input_schema")
			if !schema.Exists() || !schema.IsObject() {
				return true
			}
			originalName := strings.TrimSpace(tool.Get("name").String())
			name := toolNames.byOriginal[originalName]
			if name == "" {
				return true
			}
			declaration := []byte(`{"name":""}`)
			declaration, _ = sjson.SetBytes(declaration, "name", name)
			if description := strings.TrimSpace(tool.Get("description").String()); description != "" {
				declaration, _ = sjson.SetBytes(declaration, "description", description)
			}
			// Both paths use parameters; Antigravity's Claude route additionally
			// adapts constraints that cannot survive its input_schema bridge.
			declaration, _ = sjson.SetRawBytes(declaration, "parameters", sanitizeSchema([]byte(schema.Raw)))
			toolItems = append(toolItems, string(declaration))
			return true
		})
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
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", autoToolMode)
		case "none":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case "any":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
		case "tool":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
			if choiceName != "" {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", []string{toolNames.byOriginal[choiceName]})
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
			out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.includeThoughts", true)
		case "adaptive", "auto":
			thinkingEnabled = true
			if effort := strings.TrimSpace(gjson.GetBytes(raw, "output_config.effort").String()); effort != "" {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", geminiThinkingLevel(effort))
			} else {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", "high")
			}
			out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.includeThoughts", true)
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
			toolNameMap:       toolNames.byWire,
		},
		model:      upstreamModel,
		geminiBody: out,
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
	// the model does not imitate empty turns on "继续".
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
// functionResponse/inline_data parts using the same name map as declarations.
func geminiToolResultParts(block gjson.Result, names *geminiToolNames, requireCallID bool) []string {
	toolUseID := block.Get("tool_use_id").String()
	if toolUseID == "" {
		return nil
	}
	funcName := names.byOriginal[names.resultName(toolUseID)]

	result, resultIsRaw, images := geminiToolResultContent(block.Get("content"))
	parts := make([]string, 0, 1+len(images))
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	if id := names.resultIDForModel(toolUseID, requireCallID); id != "" {
		fr, _ = sjson.SetBytes(fr, "functionResponse.id", id)
	}
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
// blocks.
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
