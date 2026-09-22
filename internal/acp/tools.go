package acp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func toolKind(name string) string {
	base := mcpToolBase(name)
	switch strings.ToLower(base) {
	case "read":
		return "read"
	case "edit", "write", "notebookedit":
		return "edit"
	case "bash":
		return "execute"
	case "grep", "glob":
		return "search"
	case "webfetch":
		return "fetch"
	case "todowrite":
		return "think"
	default:
		return "other"
	}
}

func mcpToolBase(name string) string {
	if !strings.HasPrefix(name, "mcp__") {
		return name
	}
	parts := strings.Split(name, "__")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return name
}

func toolTitle(name string, input json.RawMessage) string {
	m := asObject(input)
	base := mcpToolBase(name)
	switch strings.ToLower(base) {
	case "edit", "write", "read":
		if p := stringField(m, "file_path", "path"); p != "" {
			return base + " " + filepath.Base(p)
		}
	case "bash":
		if c := stringField(m, "command"); c != "" {
			if len(c) > 80 {
				c = c[:80] + "..."
			}
			return c
		}
	}
	if name != "" {
		return name
	}
	return "tool"
}

func toolLocations(cwd, name string, input json.RawMessage) []toolCallLocation {
	m := asObject(input)
	p := stringField(m, "file_path", "path", "notebook_path")
	if p == "" {
		return nil
	}
	return []toolCallLocation{{Path: absPath(cwd, p)}}
}

func toolDiffs(cwd, name string, input json.RawMessage) []toolCallContent {
	m := asObject(input)
	base := strings.ToLower(mcpToolBase(name))
	switch base {
	case "edit":
		path := absPath(cwd, stringField(m, "file_path", "path"))
		if path == "" {
			return nil
		}
		old := stringField(m, "old_string")
		neu := stringField(m, "new_string")
		return []toolCallContent{{
			Type:    "diff",
			Path:    path,
			OldText: &old,
			NewText: neu,
		}}
	case "write":
		path := absPath(cwd, stringField(m, "file_path", "path"))
		if path == "" {
			return nil
		}
		neu := stringField(m, "contents", "content")
		var old *string
		if data, err := os.ReadFile(path); err == nil && len(data) <= 1<<20 {
			s := string(data)
			old = &s
		}
		return []toolCallContent{{
			Type:    "diff",
			Path:    path,
			OldText: old,
			NewText: neu,
		}}
	default:
		return nil
	}
}

func absPath(cwd, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		return p
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

func asObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func rawAsAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(append([]byte(nil), raw...))
	}
	return v
}

func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, block := range blocks {
			b.WriteString(block.Text)
		}
		return b.String()
	}
	return strings.TrimSpace(string(raw))
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *claudeImageSrc `json:"source,omitempty"`
}

type claudeImageSrc struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func promptToClaudeContent(cwd string, blocks []contentBlock, errw io.Writer) []claudeContent {
	var out []claudeContent
	for _, block := range blocks {
		switch block.Type {
		case "text", "":
			if block.Text != "" {
				out = append(out, claudeContent{Type: "text", Text: block.Text})
			}
		case "resource":
			if block.Resource != nil && block.Resource.Text != "" {
				out = append(out, claudeContent{Type: "text", Text: block.Resource.Text})
			}
		case "resource_link":
			text := strings.TrimPrefix(block.URI, "file://")
			if text == "" {
				text = block.Name
			}
			if text != "" {
				out = append(out, claudeContent{Type: "text", Text: text})
			}
		case "image":
			data, mime, ok := resolveImage(cwd, block, errw)
			if !ok {
				continue
			}
			out = append(out, claudeContent{
				Type: "image",
				Source: &claudeImageSrc{
					Type:      "base64",
					MediaType: mime,
					Data:      data,
				},
			})
		}
	}
	return out
}

func resolveImage(cwd string, block contentBlock, errw io.Writer) (data, mime string, ok bool) {
	if block.Data != "" {
		mime = block.MimeType
		if mime == "" {
			mime = "image/png"
		}
		return block.Data, mime, true
	}
	path := imagePath(cwd, block.URI)
	if path == "" {
		logf(errw, "dropping image with unsupported uri %s", block.URI)
		return "", "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		logf(errw, "dropping image %s: %v", path, err)
		return "", "", false
	}
	mime = block.MimeType
	if mime == "" {
		mime = mimeFromPath(path)
	}
	return base64.StdEncoding.EncodeToString(raw), mime, true
}

func imagePath(cwd, uri string) string {
	if uri == "" {
		return ""
	}
	if strings.HasPrefix(uri, "file://") {
		u, err := url.Parse(uri)
		if err != nil {
			return ""
		}
		return u.Path
	}
	if filepath.IsAbs(uri) {
		return uri
	}
	if cwd != "" && !strings.Contains(uri, "://") {
		return filepath.Join(cwd, uri)
	}
	return ""
}

func mimeFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func claudeTranscriptPath(cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	enc := strings.ReplaceAll(filepath.Clean(cwd), string(os.PathSeparator), "-")
	return filepath.Join(home, ".claude", "projects", enc, sessionID+".jsonl")
}

func replayTranscript(path, cwd string, emit func(any)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("transcript too large")
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		emitClaudeEvent(event, cwd, emit, true)
	}
	return nil
}

func emitClaudeEvent(event claudeStreamEvent, cwd string, emit func(any), replay bool) {
	if emit == nil {
		return
	}
	switch event.Type {
	case "assistant":
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text", "":
				if block.Text != "" {
					emit(agentMessageChunk(block.Text))
				}
			case "tool_use":
				if block.ID == "" {
					continue
				}
				emit(toolCallUpdate{
					SessionUpdate: "tool_call",
					ToolCallID:    block.ID,
					Title:         toolTitle(block.Name, block.Input),
					Name:          block.Name,
					Kind:          toolKind(block.Name),
					Status:        "in_progress",
					Locations:     toolLocations(cwd, block.Name, block.Input),
					Content:       toolDiffs(cwd, block.Name, block.Input),
					RawInput:      rawAsAny(block.Input),
				})
			}
		}
	case "user":
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text", "":
				if replay && block.Text != "" {
					emit(userMessageChunk(block.Text))
				}
			case "tool_result":
				if block.ToolUseID == "" {
					continue
				}
				status := "completed"
				if block.IsError {
					status = "failed"
				}
				text := toolResultText(block.Content)
				var content []toolCallContent
				if text != "" {
					content = []toolCallContent{{
						Type:    "content",
						Content: &contentBlock{Type: "text", Text: text},
					}}
				}
				emit(toolCallUpdate{
					SessionUpdate: "tool_call_update",
					ToolCallID:    block.ToolUseID,
					Status:        status,
					Content:       content,
					RawOutput:     text,
				})
			}
		}
	}
}
