package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type geminiBlockPos struct {
	msgIdx   int
	blockIdx int
}

type geminiPendingCompanion struct {
	pos      geminiBlockPos
	kind     string
	wireName string
	wireID   string
	text     string
}

// A single request-scoped bijection covers declarations, choice, history and
// responses. Signed history reserves its original wire names: changing those
// names would change the content authenticated by a native thought signature.
type geminiToolNames struct {
	byOriginal     map[string]string
	byWire         map[string]string
	byID           map[string]string
	replayIDs      map[string]string
	skipCompanions map[geminiBlockPos]bool
	signedCallIDs  map[geminiBlockPos]string
}

// Signed history keeps its native call ID (including an omitted ID), even
// when an older CCL response exposed a separately generated client tool ID.
// When the target model requires non-empty call IDs (such as Antigravity's
// Claude bridge), fall back to the client tool_use ID if the native call had none.
func (n *geminiToolNames) resultIDForModel(clientID string, requireCallID bool) string {
	if nativeID, ok := n.replayIDs[clientID]; ok {
		if nativeID != "" || !requireCallID {
			return nativeID
		}
	}
	return clientID
}

func (n *geminiToolNames) resultName(id string) string {
	if name := n.byID[id]; name != "" {
		return name
	}
	if name := toolNameFromClaudeToolUseID(id); name != "" {
		return name
	}
	return id
}

func buildGeminiToolNames(request *conversationRequest) (*geminiToolNames, error) {
	n := &geminiToolNames{
		byOriginal:     map[string]string{},
		byWire:         map[string]string{},
		byID:           map[string]string{},
		replayIDs:      map[string]string{},
		skipCompanions: map[geminiBlockPos]bool{},
		signedCallIDs:  map[geminiBlockPos]string{},
	}
	names := map[string]bool{}
	add := func(name string) {
		if name != "" {
			names[name] = true
		}
	}
	for _, tool := range request.Tools {
		add(strings.TrimSpace(tool.Name))
	}
	add(request.ToolChoice.Name)
	for _, turn := range request.Turns {
		if turn.Role != "assistant" {
			continue
		}
		for _, block := range turn.Parts {
			if block.Kind != partToolCall {
				continue
			}
			name := block.Name
			if name == "" {
				name = toolNameFromClaudeToolUseID(block.CallID)
			}
			n.byID[block.CallID] = name
			add(name)
		}
	}
	// Match each signed part to its synthetic companion block within the same
	// assistant turn (including adjacent assistant messages or grouped thinking
	// blocks), and clear pending companions across non-assistant boundaries.
	var pending []geminiPendingCompanion
	for msgIdx, turn := range request.Turns {
		if turn.Role != "assistant" {
			pending = nil
		}
		for blockIdx, block := range turn.Parts {
			pos := geminiBlockPos{msgIdx: msgIdx, blockIdx: blockIdx}
			if block.Kind == partToolResult {
				add(n.resultName(block.CallID))
			}
			if turn.Role != "assistant" {
				continue
			}
			if block.Kind == partReasoning {
				if encoded, ok := strings.CutPrefix(block.Signature, geminiPartSignaturePrefix); ok {
					rawPart, err := base64.StdEncoding.DecodeString(encoded)
					part := gjson.ParseBytes(rawPart)
					if err != nil || !gjson.ValidBytes(rawPart) || !part.IsObject() || !part.Get("thoughtSignature").Exists() {
						return nil, fmt.Errorf("invalid CCL Gemini part signature")
					}
					if part.Get("functionCall").Exists() {
						pending = append(pending, geminiPendingCompanion{
							pos:      pos,
							kind:     "tool_use",
							wireName: part.Get("functionCall.name").String(),
							wireID:   part.Get("functionCall.id").String(),
						})
					} else if !part.Get("thought").Bool() && part.Get("text").String() != "" {
						pending = append(pending, geminiPendingCompanion{
							pos:  pos,
							kind: "text",
							text: part.Get("text").String(),
						})
					}
				}
				continue
			}
			switch block.Kind {
			case partToolCall:
				clientID := block.CallID
				original := n.byID[clientID]
				for idx, item := range pending {
					if item.kind != "tool_use" || item.wireName == "" || original == "" {
						continue
					}
					pending = append(pending[:idx], pending[idx+1:]...)
					if prior := n.byWire[item.wireName]; prior != "" && prior != original {
						return nil, fmt.Errorf("conflicting signed Gemini tool name %q", item.wireName)
					}
					if prior := n.byOriginal[original]; prior != "" && prior != item.wireName {
						return nil, fmt.Errorf("conflicting signed Gemini aliases for tool %q", original)
					}
					n.byOriginal[original], n.byWire[item.wireName] = item.wireName, original
					n.replayIDs[clientID] = item.wireID
					n.skipCompanions[pos] = true
					n.signedCallIDs[item.pos] = clientID
					break
				}
			case partText:
				for idx, item := range pending {
					if item.kind == "text" && block.Text == item.text {
						pending = append(pending[:idx], pending[idx+1:]...)
						n.skipCompanions[pos] = true
						break
					}
				}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	// Reserve unchanged names first, so a transformed name cannot take the
	// native name of another declaration merely because of iteration order.
	for _, name := range ordered {
		if n.byOriginal[name] == "" && sanitizeFunctionName(name) == name && n.byWire[name] == "" {
			n.byOriginal[name], n.byWire[name] = name, name
		}
	}
	for _, name := range ordered {
		if n.byOriginal[name] != "" {
			continue
		}
		base := sanitizeFunctionName(name)
		wire := base
		for attempt := 0; n.byWire[wire] != ""; attempt++ {
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", name, attempt)))
			prefix := base
			if len(prefix) > 47 {
				prefix = prefix[:47]
			}
			wire = fmt.Sprintf("%s_%x", prefix, hash[:8])
		}
		n.byOriginal[name], n.byWire[wire] = wire, name
	}
	return n, nil
}
