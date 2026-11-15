package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectDefaultsStartNewConversations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	root := t.TempDir()
	base := testServer(t, Config{Token: "secret", StoreRoot: root}).URL
	var list struct {
		Workspaces []Workspace                `json:"workspaces"`
		Defaults   map[string]ProjectDefaults `json:"defaults"`
	}
	_, body := doJSON(t, "GET", base+"/api/workspaces", "secret", "")
	_ = json.Unmarshal(body, &list)
	id := list.Workspaces[0].ID

	if status, body := doJSON(t, "PUT", base+"/api/workspaces/"+id+"/defaults", "secret", `{"provider":"nope"}`); status != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d %s", status, body)
	}
	if status, body := doJSON(t, "PUT", base+"/api/workspaces/"+id+"/defaults", "secret", `{"permissionMode":"sideways"}`); status != http.StatusBadRequest {
		t.Fatalf("bad mode = %d %s", status, body)
	}
	if status, body := doJSON(t, "PUT", base+"/api/workspaces/"+id+"/defaults", "secret", `{"provider":"default","model":"opus","effort":"High","permissionMode":"plan"}`); status != http.StatusOK {
		t.Fatalf("set = %d %s", status, body)
	}
	_, body = doJSON(t, "GET", base+"/api/workspaces", "secret", "")
	_ = json.Unmarshal(body, &list)
	if got := list.Defaults[id]; got.Model != "opus" || got.Effort != "high" || got.PermissionMode != "plan" {
		t.Fatalf("listed defaults = %+v", got)
	}

	var session SessionView
	_, body = doJSON(t, "POST", base+"/api/sessions", "secret", `{"workspace":"`+id+`"}`)
	_ = json.Unmarshal(body, &session)
	if session.Provider != "default" || session.Model != "opus" || session.Effort != "high" || session.PermissionMode != "plan" {
		t.Fatalf("new conversation = %s", body)
	}
	// The request wins over the project.
	_, body = doJSON(t, "POST", base+"/api/sessions", "secret", `{"workspace":"`+id+`","model":"haiku","permissionMode":"acceptEdits"}`)
	_ = json.Unmarshal(body, &session)
	if session.Model != "haiku" || session.Effort != "high" || session.PermissionMode != "acceptEdits" {
		t.Fatalf("explicit route = %s", body)
	}

	// A provider deleted since is skipped with its model.
	data, _ := json.Marshal(map[string]ProjectDefaults{id: {Provider: "gone", Model: "x", Effort: "low"}})
	if err := os.WriteFile(filepath.Join(root, "projects.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Token: "secret", StoreRoot: root, Workspace: list.Workspaces[0].Path})
	if err != nil {
		t.Fatal(err)
	}
	base = testServerFrom(t, server)
	status, body := doJSON(t, "POST", base+"/api/sessions", "secret", `{"workspace":"`+id+`"}`)
	_ = json.Unmarshal(body, &session)
	if status != http.StatusCreated || session.Provider == "gone" || session.Model == "x" || session.Effort != "low" {
		t.Fatalf("stale provider = %d %s", status, body)
	}

	// Clearing removes the entry.
	if status, _ := doJSON(t, "PUT", base+"/api/workspaces/"+id+"/defaults", "secret", `{}`); status != http.StatusOK {
		t.Fatalf("clear = %d", status)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "projects.json")); string(data) != "{}\n" {
		t.Fatalf("projects.json = %q", data)
	}
}
