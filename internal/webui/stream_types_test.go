package webui

import (
	"os"
	"regexp"
	"testing"
)

// The page's EventSource listens for event types by name and drops the rest,
// so a type the server stores but the page does not list only shows after a
// reload.
func TestPageStreamsEveryEventType(t *testing.T) {
	source, err := os.ReadFile("frontend/src/useConversation.ts")
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`const STREAM_TYPES = \[([^\]]*)\]`).FindSubmatch(source)
	if list == nil {
		t.Fatal("STREAM_TYPES not found in useConversation.ts")
	}
	listed := map[string]bool{}
	for _, match := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(list[1], -1) {
		listed[string(match[1])] = true
	}
	for _, kind := range []string{
		EventInit, EventToken, EventThinking, EventContext, EventToolUse, EventToolResult, EventTask,
		EventPermission, EventPermissionResolved, EventResult, EventError, EventQueued, EventUser, EventStatus,
	} {
		if !listed[kind] {
			t.Errorf("the page does not stream %q events", kind)
		}
	}
}
