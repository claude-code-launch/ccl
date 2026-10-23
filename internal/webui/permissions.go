package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Interactive permissions. A turn that does not bypass permissions runs with
// --permission-prompt-tool stdio: Claude Code sends each approval it needs as
// a can_use_tool control request on stdout, and the page's answer goes back
// on stdin as a control response.

const (
	EventPermission         = "permission"
	EventPermissionResolved = "permission_resolved"
	// eventUnsupportedControl is internal: a control request nothing here
	// answers. It is never stored.
	eventUnsupportedControl = "control_unsupported"
)

// PermissionModes are the --permission-mode values a conversation can use.
var PermissionModes = []string{"default", "acceptEdits", "plan", "bypassPermissions"}

var (
	errTurnEnded       = errors.New("the turn has ended")
	errUnknownRequest  = errors.New("no such permission request: it was answered or withdrawn")
	errInvalidDecision = errors.New(`behavior must be "allow" or "deny"`)
)

// PermissionPayload is one approval Claude Code asks for.
type PermissionPayload struct {
	RequestID string `json:"requestId"`
	ToolName  string `json:"toolName"`
	ToolUseID string `json:"toolUseId,omitempty"`
	// Title and Description are Claude Code's own wording, when it gives one.
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	BlockedPath string          `json:"blockedPath,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	// CanRemember says Claude Code offered rules that would stop asking.
	CanRemember bool `json:"canRemember,omitempty"`

	// raw and suggestions stay in memory: answers send the original input back.
	raw         json.RawMessage
	suggestions json.RawMessage
}

// PermissionResolvedPayload closes a permission request.
type PermissionResolvedPayload struct {
	RequestID string `json:"requestId"`
	// Behavior is allow, deny or cancelled (Claude Code withdrew the request).
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// PermissionDecision is the user's answer.
type PermissionDecision struct {
	RequestID string `json:"requestId"`
	Behavior  string `json:"behavior"`
	// Message tells Claude why a call was refused.
	Message string `json:"message"`
	// Answers holds AskUserQuestion answers, question text → chosen label(s).
	Answers map[string]string `json:"answers"`
	// Remember applies Claude Code's suggested rules for the rest of the session.
	Remember bool `json:"remember"`
	// Mode switches the permission mode, as approving a plan does.
	Mode string `json:"mode"`
}

func validPermissionMode(mode string) bool {
	for _, candidate := range PermissionModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// handleControlRequest translates a control request from Claude Code.
func (t *translator) handleControlRequest(requestID string, request json.RawMessage, emit emitter) {
	var body struct {
		Subtype        string          `json:"subtype"`
		ToolName       string          `json:"tool_name"`
		Input          json.RawMessage `json:"input"`
		ToolUseID      string          `json:"tool_use_id"`
		Title          string          `json:"title"`
		Description    string          `json:"description"`
		DecisionReason string          `json:"decision_reason"`
		BlockedPath    string          `json:"blocked_path"`
		Suggestions    json.RawMessage `json:"permission_suggestions"`
	}
	if requestID == "" || json.Unmarshal(request, &body) != nil {
		return
	}
	if body.Subtype != "can_use_tool" {
		emit(eventUnsupportedControl, requestID)
		return
	}
	description := body.Description
	if description == "" {
		description = toolDescription(body.Input)
	}
	suggestions := body.Suggestions
	if string(suggestions) == "null" || string(suggestions) == "[]" {
		suggestions = nil
	}
	emit(EventPermission, PermissionPayload{
		RequestID:   requestID,
		ToolName:    body.ToolName,
		ToolUseID:   body.ToolUseID,
		Title:       truncateRunes(redactSecrets(body.Title), SummaryLimit),
		Description: truncateRunes(redactSecrets(description), SummaryLimit),
		Summary:     summarizeToolInput(body.ToolName, body.Input),
		Reason:      truncateRunes(redactSecrets(body.DecisionReason), 1000),
		BlockedPath: body.BlockedPath,
		Input:       toolInput(body.Input),
		CanRemember: len(suggestions) > 0,
		raw:         body.Input,
		suggestions: suggestions,
	})
}

func controlError(requestID, message string) []byte {
	line, _ := json.Marshal(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "error", "request_id": requestID, "error": message},
	})
	return append(line, '\n')
}

// permissionResponse is the control response for a decision.
func permissionResponse(request pendingPermission, decision PermissionDecision) ([]byte, error) {
	var body map[string]any
	switch decision.Behavior {
	case "allow":
		input := map[string]any{}
		if len(request.input) > 0 {
			if err := json.Unmarshal(request.input, &input); err != nil {
				return nil, fmt.Errorf("decode tool input: %w", err)
			}
		}
		if len(decision.Answers) > 0 {
			input["answers"] = decision.Answers
		}
		body = map[string]any{"behavior": "allow", "updatedInput": input}
		var permissions []any
		if decision.Remember && len(request.suggestions) > 0 {
			var suggestions []map[string]any
			if err := json.Unmarshal(request.suggestions, &suggestions); err == nil {
				for _, suggestion := range suggestions {
					// Claude Code may suggest writing a settings file; from the
					// browser the rule only lasts for this session.
					suggestion["destination"] = "session"
					permissions = append(permissions, suggestion)
				}
			}
		}
		if decision.Mode != "" {
			permissions = append(permissions, map[string]any{"type": "setMode", "mode": decision.Mode, "destination": "session"})
		}
		if len(permissions) > 0 {
			body["updatedPermissions"] = permissions
		}
	case "deny":
		message := strings.TrimSpace(decision.Message)
		if message == "" {
			message = "The user declined this in ccl serve."
		}
		body = map[string]any{"behavior": "deny", "message": message}
	default:
		return nil, errInvalidDecision
	}
	line, err := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": decision.RequestID,
			"response":   body,
		},
	})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// Respond answers a permission request of the session's running turn.
func (t *Timeline) Respond(session *Session, decision PermissionDecision) error {
	if decision.Mode != "" && !validPermissionMode(decision.Mode) {
		return fmt.Errorf("unknown permission mode %q", decision.Mode)
	}
	t.mu.Lock()
	entry := t.running[session.ID()]
	t.mu.Unlock()
	if entry == nil {
		return errTurnEnded
	}
	entry.mu.Lock()
	request, ok := entry.pending[decision.RequestID]
	if ok {
		delete(entry.pending, decision.RequestID)
	}
	entry.mu.Unlock()
	if !ok {
		return errUnknownRequest
	}
	line, err := permissionResponse(request, decision)
	if err != nil {
		entry.mu.Lock()
		if entry.pending != nil {
			entry.pending[decision.RequestID] = request
		}
		entry.mu.Unlock()
		return err
	}
	if err := entry.write(line); err != nil {
		return err
	}
	if decision.Mode != "" {
		_, _ = session.UpdateMeta(func(meta *Meta) { meta.PermissionMode = decision.Mode })
	}
	behavior := decision.Behavior
	_, _ = session.Append(EventPermissionResolved, PermissionResolvedPayload{
		RequestID: decision.RequestID, Behavior: behavior, Message: strings.TrimSpace(decision.Message), Mode: decision.Mode,
	})
	return nil
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request, session *Session) {
	var decision PermissionDecision
	if err := decodeBody(r, &decision); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch err := s.timeline.Respond(session, decision); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, errTurnEnded):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errUnknownRequest):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
