package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// statuslinePayload is the subset of Claude Code's status-line input that ccl
// renders. Claude Code pipes one JSON object per refresh; unknown fields are
// ignored so a newer Claude Code cannot break the line.
type statuslinePayload struct {
	Model struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	// Effort is either a bare string ("xhigh") or an object ({"level":"xhigh"}),
	// so it is decoded lazily.
	Effort json.RawMessage `json:"effort"`
	// UsedPercentage is a pointer so a reported 0 renders as "0%" while an absent
	// context window is omitted entirely.
	ContextWindow struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
}

// runStatusline renders ccl's status line from the JSON Claude Code writes to
// stdin. It is reached by the shortcut at the top of Execute, before any config
// is read: Claude Code runs this command on every status-line refresh, so it
// must stay side-effect free and never touch ~/.ccl/config.yaml.
//
// Errors never surface: a malformed payload prints an empty line so Claude Code
// shows nothing rather than an error string.
func runStatusline() {
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	if line := formatStatusline(payload); line != "" {
		fmt.Println(line)
	}
}

// formatStatusline renders "model · effort · NN%" from a status-line payload,
// dropping empty segments. It returns "" when nothing can be shown.
func formatStatusline(payload []byte) string {
	var parsed statuslinePayload
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return ""
	}

	parts := make([]string, 0, 3)
	if model := statuslineModel(parsed); model != "" {
		parts = append(parts, model)
	}
	if effort := statuslineEffort(parsed.Effort); effort != "" {
		parts = append(parts, effort)
	}
	if parsed.ContextWindow.UsedPercentage != nil {
		parts = append(parts, fmt.Sprintf("%.0f%%", *parsed.ContextWindow.UsedPercentage))
	}
	return strings.Join(parts, " · ")
}

// statuslineModel prefers the display name, falling back to the technical ID.
// Claude Code reports "Auto" as the display name when ccl has not pinned a
// model, which is less useful than the resolved ID, so it is skipped.
func statuslineModel(parsed statuslinePayload) string {
	display := cleanStatuslineField(parsed.Model.DisplayName)
	if display != "" && !strings.EqualFold(display, "Auto") {
		return display
	}
	return cleanStatuslineField(parsed.Model.ID)
}

// statuslineEffort accepts both shapes Claude Code has used for the reasoning
// effort field: a bare string, or an object carrying a "level".
func statuslineEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var level string
	if err := json.Unmarshal(raw, &level); err == nil {
		return cleanStatuslineField(level)
	}
	var object struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return cleanStatuslineField(object.Level)
	}
	return ""
}

// cleanStatuslineField trims a value and drops any trailing structured suffix.
// Claude Code has been seen appending an effort blob to the display name, e.g.
// `z-ai/glm-5.3-free({"level": "xhigh"})`, which must not reach the status line.
func cleanStatuslineField(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexAny(value, "({"); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}
