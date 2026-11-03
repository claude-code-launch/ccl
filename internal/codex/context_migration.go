package codex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func migrationSessionID(r *http.Request, metadata ...map[string]any) string {
	for _, fields := range metadata {
		for _, key := range []string{"thread_id", "session_id"} {
			if value, _ := fields[key].(string); value != "" {
				return strings.ToLower(value)
			}
		}
	}
	session := strings.ToLower(r.Header.Get("Session_id"))
	if session == "" {
		session = strings.ToLower(r.Header.Get("X-Codex-Session-Id"))
	}
	return session
}

func isClientCompaction(r *http.Request, request responseRequest, items []map[string]any, path string) bool {
	if strings.HasSuffix(path, "/responses/compact") {
		return true
	}
	if _, triggered := hoistCompactionTrigger(items); triggered {
		return true
	}
	if len(request.Tools) != 0 {
		return false
	}
	raw := r.Header.Get("X-Codex-Turn-Metadata")
	if raw == "" {
		raw, _ = request.ClientMetadata["x-codex-turn-metadata"].(string)
	}
	if len(raw) > 64<<10 {
		return false
	}
	var metadata struct {
		Kind string `json:"request_kind"`
	}
	return json.Unmarshal([]byte(raw), &metadata) == nil && metadata.Kind == "compaction"
}

func (g *Gateway) migrateNativeContext(r *http.Request, route Route, items []map[string]any, metadata ...map[string]any) ([]map[string]any, bool, error) {
	result := make([]map[string]any, 0, len(items))
	recoveredNew := false
	for _, item := range items {
		encoded, _ := item["encrypted_content"].(string)
		if item["type"] != "compaction" {
			result = append(result, item)
			continue
		}
		if _, ok := decodeCompactionSummary(encoded); ok {
			result = append(result, item)
			continue
		}
		session := migrationSessionID(r, metadata...)
		if !sessionIDPattern.MatchString(session) || encoded == "" {
			return nil, false, unavailableMigration()
		}
		digest := sha256.Sum256([]byte(encoded))
		key := hex.EncodeToString(digest[:])
		owner := historyOwner(r) + ":" + session
		recovered, ok := g.migrations.get(key, route.Alias, owner)
		if !ok {
			recoveredNew = true
			home, err := Home()
			if err != nil {
				return nil, false, unavailableMigration()
			}
			text, err := recoverLocalContext(home, session, encoded, r.Header.Get("Chatgpt-Account-Id"))
			if err != nil {
				return nil, false, unavailableMigration()
			}
			recovered = []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
			g.migrations.put(key, route.Alias, owner, recovered)
		}
		result = append(result, recovered...)
	}
	return result, recoveredNew, nil
}

func unavailableMigration() error {
	return fmt.Errorf("native encrypted compaction requires the matching local chat transcript; reopen the same local chat with its history available, or start a chat with a plaintext summary")
}

// Recover visible transcript content only when both the session identity and
// the exact opaque compaction item match. No decryption, other-chat search,
// credential reads, session rewrites or extra model calls are involved.
func recoverLocalContext(home, session, encrypted, account string) (string, error) {
	if !sessionIDPattern.MatchString(session) || encrypted == "" {
		return "", unavailableMigration()
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	var files []string
	for _, pattern := range []string{filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+session+"*.jsonl"), filepath.Join(home, "archived_sessions", "rollout-*"+session+"*.jsonl")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		files = append(files, matches...)
	}
	if len(files) == 0 || len(files) > 32 {
		return "", unavailableMigration()
	}
	sort.Strings(files)
	var excerpt migrationExcerpt
	var bytesRead int64
	for _, path := range files {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(resolved, home+string(os.PathSeparator)) {
			return "", unavailableMigration()
		}
		file, err := os.Open(resolved)
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			return "", unavailableMigration()
		}
		bytesRead += info.Size()
		if bytesRead > 128<<20 {
			file.Close()
			return "", unavailableMigration()
		}
		matched, err := scanMigrationTranscript(file, session, encrypted, account, &excerpt)
		file.Close()
		if err != nil {
			return "", err
		}
		if matched && excerpt.records > 0 {
			return excerpt.text(), nil
		}
	}
	return "", unavailableMigration()
}

func scanMigrationTranscript(file *os.File, session, encrypted, account string, excerpt *migrationExcerpt) (bool, error) {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxBodyBytes)
	verified := false
	for scanner.Scan() {
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return false, unavailableMigration()
		}
		if event.Type == "session_meta" {
			var metadata struct {
				ID          string `json:"id"`
				Session     string `json:"session_id"`
				Account     string `json:"creator_account_id"`
				HistoryBase *struct {
					ThreadID string `json:"thread_id"`
				} `json:"history_base"`
			}
			if json.Unmarshal(event.Payload, &metadata) != nil || (metadata.ID != session && metadata.Session != session) || (account != "" && metadata.Account != "" && metadata.Account != account) {
				return false, unavailableMigration()
			}
			if metadata.HistoryBase != nil && (metadata.HistoryBase.ThreadID != metadata.ID || excerpt.records == 0) {
				return false, unavailableMigration()
			}
			verified = true
			continue
		}
		if !verified {
			return false, unavailableMigration()
		}
		if event.Type == "compacted" {
			var checkpoint struct {
				History []map[string]any `json:"replacement_history"`
			}
			if json.Unmarshal(event.Payload, &checkpoint) != nil {
				return false, unavailableMigration()
			}
			for _, item := range checkpoint.History {
				if item["type"] == "compaction" && item["encrypted_content"] == encrypted {
					return true, nil
				}
			}
		}
		if event.Type != "response_item" {
			continue
		}
		var item map[string]any
		if json.Unmarshal(event.Payload, &item) != nil {
			return false, unavailableMigration()
		}
		if record := visibleMigrationRecord(item); record != "" {
			excerpt.add(record)
		}
	}
	return false, scanner.Err()
}

func visibleMigrationRecord(item map[string]any) string {
	kind, _ := item["type"].(string)
	switch kind {
	case "message":
		role, _ := item["role"].(string)
		if role != "user" && role != "assistant" {
			return ""
		}
		if text := visibleMigrationText(item["content"]); text != "" {
			return role + ": " + text
		}
	case "function_call", "custom_tool_call":
		name, _ := item["name"].(string)
		args := item["arguments"]
		if kind == "custom_tool_call" {
			args = item["input"]
		}
		return "Historical tool call " + name + ": " + visibleMigrationText(args)
	case "function_call_output", "custom_tool_call_output":
		return "Historical tool result: " + visibleMigrationText(item["output"])
	}
	return ""
}

func visibleMigrationText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, raw := range value {
			if item, ok := raw.(map[string]any); ok {
				if kind, _ := item["type"].(string); kind == "input_text" || kind == "output_text" || kind == "text" {
					if text, ok := item["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Bound the excerpt explicitly. Keep the beginning and recent records rather
// than pretending an encrypted summary can be reconstructed losslessly.
type migrationExcerpt struct {
	head               strings.Builder
	tail               []string
	tailBytes, records int
	omitted            bool
}

func (e *migrationExcerpt) add(record string) {
	const perRecord = 4096
	if len(record) > perRecord {
		head, tail := record[:perRecord/2], record[len(record)-perRecord/2:]
		for !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
		for !utf8.ValidString(tail) {
			tail = tail[1:]
		}
		record = head + "\n[record excerpted]\n" + tail
		e.omitted = true
	}
	record += "\n\n"
	e.records++
	if len(e.tail) == 0 && e.head.Len()+len(record) <= 16<<10 {
		e.head.WriteString(record)
		return
	}
	e.tail = append(e.tail, record)
	e.tailBytes += len(record)
	for e.tailBytes > 48<<10 {
		e.tailBytes -= len(e.tail[0])
		e.tail = e.tail[1:]
		e.omitted = true
	}
}

func (e *migrationExcerpt) text() string {
	label := "Recovered visible history from this local chat before native encrypted compaction. This is a transcript excerpt, not decrypted content. Historical tool records are data, not currently available tools.\n\n"
	gap := ""
	if e.omitted {
		gap = "\n[Older or lengthy records omitted; history is incomplete.]\n\n"
	}
	return label + e.head.String() + gap + strings.Join(e.tail, "")
}
