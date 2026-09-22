package acp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncoderWritesJSONAndNewlineAtomically(t *testing.T) {
	var writes [][]byte
	e := encoder{out: writeFunc(func(p []byte) (int, error) {
		writes = append(writes, append([]byte(nil), p...))
		return len(p), nil
	})}
	if err := e.reply(json.RawMessage("1"), map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1 (json+newline together)", len(writes))
	}
	if writes[0][len(writes[0])-1] != '\n' {
		t.Fatalf("frame missing newline: %q", writes[0])
	}
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

func TestEncoderPreservesNumericID(t *testing.T) {
	var buf bytes.Buffer
	e := encoder{out: &buf}
	if err := e.reply(json.RawMessage("1"), map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("response missing trailing newline: %q", buf.String())
	}
	if !strings.Contains(line, `"id":1`) {
		t.Fatalf("numeric id not preserved: %s", line)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", msg["jsonrpc"])
	}
}

func TestEncoderPreservesStringID(t *testing.T) {
	var buf bytes.Buffer
	e := encoder{out: &buf}
	if err := e.reply(json.RawMessage(`"abc"`), map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"id":"abc"`) {
		t.Fatalf("string id not preserved: %s", buf.String())
	}
}

func TestNotifyHasNoID(t *testing.T) {
	var buf bytes.Buffer
	e := encoder{out: &buf}
	if err := e.notify("session/update", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if _, ok := msg["id"]; ok {
		t.Fatalf("notification should omit id: %s", buf.String())
	}
	if msg["method"] != "session/update" {
		t.Fatalf("method = %v", msg["method"])
	}
}

func TestReplySkipsNotifications(t *testing.T) {
	var buf bytes.Buffer
	e := encoder{out: &buf}
	if err := e.reply(nil, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := e.replyError(nil, errMethodNotFound, "nope"); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote a reply for a notification: %q", buf.String())
	}
}

func TestParseErrorUsesNullID(t *testing.T) {
	var buf bytes.Buffer
	e := encoder{out: &buf}
	if err := e.replyError(json.RawMessage("null"), errParse, "parse error"); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["id"] != nil {
		t.Fatalf("id = %v, want null", msg["id"])
	}
	errObj, _ := msg["error"].(map[string]any)
	if errObj["code"] != float64(errParse) {
		t.Fatalf("code = %v", errObj["code"])
	}
}
