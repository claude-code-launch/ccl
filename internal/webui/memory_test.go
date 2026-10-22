package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestUserMemoryCreatesAndRefusesStaleSaves(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	base := testServer(t, Config{Token: "secret", Workspace: t.TempDir()}).URL

	var memory UserMemory
	status, body := doJSON(t, "GET", base+"/api/memory", "secret", "")
	if status != http.StatusOK || json.Unmarshal(body, &memory) != nil || memory.Hash != "" || memory.Path != filepath.Join(config, "CLAUDE.md") {
		t.Fatalf("missing memory = %d %s", status, body)
	}
	// An empty hash creates the file.
	status, body = doJSON(t, "PUT", base+"/api/memory", "secret", `{"content":"be brief\n","expectedHash":""}`)
	if status != http.StatusOK {
		t.Fatalf("create = %d %s", status, body)
	}
	var saved struct{ Hash string }
	_ = json.Unmarshal(body, &saved)
	if data, _ := os.ReadFile(filepath.Join(config, "CLAUDE.md")); string(data) != "be brief\n" {
		t.Fatalf("file = %q", data)
	}
	// Creating again would overwrite what was just written.
	if status, body = doJSON(t, "PUT", base+"/api/memory", "secret", `{"content":"x","expectedHash":""}`); status != http.StatusConflict {
		t.Fatalf("second create = %d %s", status, body)
	}
	if status, body = doJSON(t, "PUT", base+"/api/memory", "secret", `{"content":"be terse\n","expectedHash":"`+saved.Hash+`"}`); status != http.StatusOK {
		t.Fatalf("update = %d %s", status, body)
	}
	status, body = doJSON(t, "GET", base+"/api/memory", "secret", "")
	if json.Unmarshal(body, &memory) != nil || memory.Content != "be terse\n" || memory.Hash == "" {
		t.Fatalf("read back = %d %s", status, body)
	}
	if status, _ = doJSON(t, "DELETE", base+"/api/memory", "secret", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("delete = %d", status)
	}
	if status, _ = doJSON(t, "GET", base+"/api/memory", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("no token = %d", status)
	}
}
