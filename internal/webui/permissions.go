package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// Interactive permissions over ACP v1 RequestPermission. When a turn runs in a
// non-bypass permission mode, Grok Build requests approval before tools that
// require confirmation, and the browser's decision selects the matching ACP
// permission option.

const (
	EventPermission         = "permission"
	EventPermissionResolved = "permission_resolved"
)

// PermissionModes are the permission modes a conversation can use.
var PermissionModes = []string{"default", "acceptEdits", "plan", "bypassPermissions"}

var (
	errTurnEnded       = errors.New("the turn has ended")
	errUnknownRequest  = errors.New("no such permission request: it was answered or withdrawn")
	errInvalidDecision = errors.New(`behavior must be "allow" or "deny"`)
)

// PermissionPayload is one approval Grok Build asks for.
type PermissionPayload struct {
	RequestID   string          `json:"requestId"`
	ToolName    string          `json:"toolName"`
	ToolUseID   string          `json:"toolUseId,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	BlockedPath string          `json:"blockedPath,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	// CanRemember reports whether Grok Build offered an allow_always option.
	CanRemember bool `json:"canRemember,omitempty"`
}

// PermissionResolvedPayload closes a permission request.
type PermissionResolvedPayload struct {
	RequestID string `json:"requestId"`
	// Behavior is allow, deny or cancelled.
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// PermissionDecision is the user's answer.
type PermissionDecision struct {
	RequestID string `json:"requestId"`
	Behavior  string `json:"behavior"`
	// Message records why a call was refused in the session history.
	Message string `json:"message"`
	// Answers holds AskUserQuestion answers when present.
	Answers map[string]string `json:"answers"`
	// Remember selects allow_always when the agent offered it.
	Remember bool `json:"remember"`
	// Mode switches the conversation's permission mode for subsequent turns.
	Mode string `json:"mode"`
}

type pendingPermission struct {
	allowOnce    acp.PermissionOptionId
	allowAlways  acp.PermissionOptionId
	rejectOnce   acp.PermissionOptionId
	rejectAlways acp.PermissionOptionId
	reply        chan PermissionDecision
}

func (p pendingPermission) optionFor(decision PermissionDecision) (acp.PermissionOptionId, bool) {
	switch decision.Behavior {
	case "allow":
		if decision.Remember && p.allowAlways != "" {
			return p.allowAlways, true
		}
		if p.allowOnce != "" {
			return p.allowOnce, true
		}
		if p.allowAlways != "" {
			return p.allowAlways, true
		}
	case "deny":
		if p.rejectOnce != "" {
			return p.rejectOnce, true
		}
		if p.rejectAlways != "" {
			return p.rejectAlways, true
		}
	}
	return "", false
}

func buildPermissionRequest(requestID string, req acp.RequestPermissionRequest) (PermissionPayload, pendingPermission) {
	pending := pendingPermission{reply: make(chan PermissionDecision, 1)}
	for _, opt := range req.Options {
		switch opt.Kind {
		case acp.PermissionOptionKindAllowOnce:
			if pending.allowOnce == "" {
				pending.allowOnce = opt.OptionId
			}
		case acp.PermissionOptionKindAllowAlways:
			if pending.allowAlways == "" {
				pending.allowAlways = opt.OptionId
			}
		case acp.PermissionOptionKindRejectOnce:
			if pending.rejectOnce == "" {
				pending.rejectOnce = opt.OptionId
			}
		case acp.PermissionOptionKindRejectAlways:
			if pending.rejectAlways == "" {
				pending.rejectAlways = opt.OptionId
			}
		}
	}

	var kind acp.ToolKind
	if req.ToolCall.Kind != nil {
		kind = *req.ToolCall.Kind
	}
	var title string
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	name, desc, summary, input := acpToolPresentationWithMeta(kind, title, req.ToolCall.RawInput, req.ToolCall.Locations, req.ToolCall.Content, req.ToolCall.Meta)
	blockedPath := ""
	if len(req.ToolCall.Locations) > 0 {
		blockedPath = req.ToolCall.Locations[0].Path
	}

	payload := PermissionPayload{
		RequestID:   requestID,
		ToolName:    name,
		ToolUseID:   string(req.ToolCall.ToolCallId),
		Title:       truncateRunes(redactSecrets(title), SummaryLimit),
		Description: desc,
		Summary:     summary,
		BlockedPath: blockedPath,
		Input:       input,
		CanRemember: pending.allowAlways != "",
	}
	return payload, pending
}

func permissionOutcomeMeta(decision PermissionDecision) map[string]any {
	var meta map[string]any
	if len(decision.Answers) > 0 {
		answers := make(map[string]string, len(decision.Answers))
		for k, v := range decision.Answers {
			answers[k] = v
		}
		meta = map[string]any{"answers": answers}
	}
	if msg := strings.TrimSpace(decision.Message); msg != "" {
		if meta == nil {
			meta = make(map[string]any, 1)
		}
		meta["message"] = msg
	}
	return meta
}

func validPermissionMode(mode string) bool {
	for _, candidate := range PermissionModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// Respond answers a permission request of the session's running turn.
func (t *Timeline) Respond(session *Session, decision PermissionDecision) error {
	if decision.Behavior != "allow" && decision.Behavior != "deny" {
		return errInvalidDecision
	}
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
	closed := entry.closed
	request, ok := entry.pending[decision.RequestID]
	if ok && !closed {
		delete(entry.pending, decision.RequestID)
	}
	entry.mu.Unlock()
	if closed {
		return errTurnEnded
	}
	if !ok {
		return errUnknownRequest
	}
	select {
	case request.reply <- decision:
	default:
	}
	if decision.Mode != "" {
		_, _ = session.UpdateMeta(func(meta *Meta) { meta.PermissionMode = decision.Mode })
	}
	_, _ = session.Append(EventPermissionResolved, PermissionResolvedPayload{
		RequestID: decision.RequestID,
		Behavior:  decision.Behavior,
		Message:   strings.TrimSpace(decision.Message),
		Mode:      decision.Mode,
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
