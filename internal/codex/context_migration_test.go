package codex

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const migrationSession = "01912345-6789-7000-8000-123456789abc"

func writeMigrationFixture(t *testing.T, home, session, encrypted string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11", "03")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-03T00-00-00-"+session+".jsonl")
	var records []map[string]any
	records = append(records, map[string]any{"type": "session_meta", "payload": map[string]any{"id": session, "creator_account_id": "fake-native-account"}})
	for _, item := range []map[string]any{
		{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "keep-this-goal"}}},
		{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "private-reasoning-not-for-migration"}}},
		{"type": "function_call", "name": "old_tool", "arguments": "old tool arguments"},
		{"type": "function_call_output", "call_id": "old-call", "output": "prior-test-result"},
		{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "pending-fix"}}},
	} {
		records = append(records, map[string]any{"type": "response_item", "payload": item})
	}
	records = append(records, map[string]any{"type": "compacted", "payload": map[string]any{"message": nil, "replacement_history": []any{map[string]any{"type": "compaction", "encrypted_content": encrypted}}}})
	records = append(records, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "future-message-must-not-leak"}}}})
	var data []byte
	for _, record := range records {
		line, _ := json.Marshal(record)
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeCompactionMigrationValidatesChatAndCiphertext(t *testing.T) {
	home := t.TempDir()
	writeMigrationFixture(t, home, migrationSession, "opaque-test-cipher")
	text, err := recoverLocalContext(home, migrationSession, "opaque-test-cipher", "fake-native-account")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"keep-this-goal", "prior-test-result", "pending-fix", "not decrypted"} {
		if !strings.Contains(text, want) {
			t.Errorf("lost context %q", want)
		}
	}
	for _, unwanted := range []string{"private-reasoning-not-for-migration", "future-message-must-not-leak", "opaque-test-cipher"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("migration leaked %q", unwanted)
		}
	}
	for _, test := range []struct{ session, cipher, account string }{
		{migrationSession, "other-cipher", "fake-native-account"},
		{migrationSession, "opaque-test-cipher", "other-account"},
		{"01912345-6789-7000-8000-123456789abd", "opaque-test-cipher", "fake-native-account"},
	} {
		if _, err := recoverLocalContext(home, test.session, test.cipher, test.account); err == nil {
			t.Fatal("unrelated context was accepted")
		}
	}
}

func TestNativeCompactionMigrationContinuesConvertedConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeMigrationFixture(t, home, migrationSession, "opaque-test-cipher")
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text := textValue(body["messages"])
		if !strings.Contains(text, "keep-this-goal") || !strings.Contains(text, "prior-test-result") || strings.Contains(text, "opaque-test-cipher") {
			t.Error("local context was not migrated safely")
		}
		if tools, _ := body["tools"].([]any); len(tools) != 0 {
			t.Error("historical tool was reauthorized")
		}
		writeMessage(w, []map[string]any{{"type": "text", "text": "continued-from-local-history"}})
	}, "anthropic", Options{Desktop: true})
	for _, stream := range []bool{false, true} {
		requestBody := map[string]any{"model": "ccl/chosen/model-a", "stream": stream, "input": []any{map[string]any{"type": "compaction", "encrypted_content": "opaque-test-cipher"}, map[string]any{"role": "user", "content": "continue"}}}
		raw, _ := json.Marshal(requestBody)
		r, _ := http.NewRequest("POST", g.BaseURL()+"/responses", strings.NewReader(string(raw)))
		r.Header.Set("Session_id", migrationSession)
		r.Header.Set("Chatgpt-Account-Id", "fake-native-account")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(data), "continued-from-local-history") {
			t.Fatalf("migration failed: %d %s", response.StatusCode, data)
		}
		if !stream && !strings.Contains(string(data), "older details may be missing") {
			t.Fatal("migration limitation was hidden")
		}
	}
}

func TestNativeCompactionMigrationRejectsMissingIdentityAndOutsideFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := writeMigrationFixture(t, home, migrationSession, "opaque")
	outside := t.TempDir()
	target := writeMigrationFixture(t, outside, migrationSession, "opaque")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symbolic link permissions unavailable")
		}
		t.Fatal(err)
	}
	if _, err := recoverLocalContext(home, migrationSession, "opaque", ""); err == nil {
		t.Fatal("followed transcript outside Codex home")
	}
	g := &Gateway{}
	r, _ := http.NewRequest("POST", "http://127.0.0.1/", nil)
	for _, session := range []string{"", "../../other-chat", "*"} {
		r.Header.Set("Session_id", session)
		if _, _, err := g.migrateNativeContext(r, Route{}, []map[string]any{{"type": "compaction", "encrypted_content": "opaque"}}); err == nil {
			t.Fatal("missing/invalid chat identity was accepted")
		}
	}
}

func TestMigrationExcerptIsBoundedAndDisclosesTruncation(t *testing.T) {
	var excerpt migrationExcerpt
	excerpt.add("first-goal")
	for i := 0; i < 1000; i++ {
		excerpt.add(strings.Repeat("é🙂", 1000))
	}
	excerpt.add("latest-state")
	text := excerpt.text()
	if len(text) > 70<<10 || !strings.Contains(text, "history is incomplete") || !strings.Contains(text, "first-goal") || !strings.Contains(text, "latest-state") {
		t.Fatal("excerpt lost bounds or truncation notice")
	}
}
