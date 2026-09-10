package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// A single request-scoped bijection covers declarations, choice, history and
// responses. Signed history reserves its original wire names: changing those
// names would change the content authenticated by a native thought signature.
type geminiToolNames struct {
	byOriginal map[string]string
	byWire     map[string]string
	byID       map[string]string
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

func buildGeminiToolNames(raw []byte) (*geminiToolNames, error) {
	n := &geminiToolNames{byOriginal: map[string]string{}, byWire: map[string]string{}, byID: map[string]string{}}
	names := map[string]bool{}
	add := func(name string) {
		if name != "" {
			names[name] = true
		}
	}
	for _, tool := range gjson.GetBytes(raw, "tools").Array() {
		add(strings.TrimSpace(tool.Get("name").String()))
	}
	add(gjson.GetBytes(raw, "tool_choice.name").String())
	messages := gjson.GetBytes(raw, "messages").Array()
	for _, message := range messages {
		if message.Get("role").String() != "assistant" {
			continue
		}
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() != "tool_use" {
				continue
			}
			name := block.Get("name").String()
			if name == "" {
				name = toolNameFromClaudeToolUseID(block.Get("id").String())
			}
			n.byID[block.Get("id").String()] = name
			add(name)
		}
	}
	// Match each signed functionCall to its synthetic tool_use companion, even
	// when a client persists adjacent blocks as separate assistant messages.
	pendingWire := ""
	for _, message := range messages {
		if message.Get("role").String() != "assistant" {
			pendingWire = ""
		}
		for _, block := range message.Get("content").Array() {
			kind := block.Get("type").String()
			if kind == "tool_result" {
				add(n.resultName(block.Get("tool_use_id").String()))
			}
			if message.Get("role").String() != "assistant" {
				continue
			}
			if pendingWire != "" {
				if kind == "tool_use" {
					original := n.byID[block.Get("id").String()]
					if original != "" {
						if prior := n.byWire[pendingWire]; prior != "" && prior != original {
							return nil, fmt.Errorf("conflicting signed Gemini tool name %q", pendingWire)
						}
						if prior := n.byOriginal[original]; prior != "" && prior != pendingWire {
							return nil, fmt.Errorf("conflicting signed Gemini aliases for tool %q", original)
						}
						n.byOriginal[original], n.byWire[pendingWire] = pendingWire, original
					}
				}
				pendingWire = ""
			}
			if kind == "thinking" {
				if encoded, ok := strings.CutPrefix(block.Get("signature").String(), geminiPartSignaturePrefix); ok {
					part, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil || !gjson.ValidBytes(part) {
						return nil, fmt.Errorf("invalid CCL Gemini part signature")
					}
					pendingWire = gjson.GetBytes(part, "functionCall.name").String()
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
