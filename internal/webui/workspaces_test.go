package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type served struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func listWorkspaces(t *testing.T, base string) []served {
	t.Helper()
	_, body := doJSON(t, "GET", base+"/api/workspaces", "secret", "")
	var result struct {
		Workspaces []served `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("workspaces = %s", body)
	}
	return result.Workspaces
}

func TestWorkspaceRootsOfferTheirSubdirectories(t *testing.T) {
	fixed := t.TempDir()
	root := t.TempDir()
	for _, name := range []string{"beta", "alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	httpServer := testServer(t, Config{Token: "secret", Workspaces: []string{fixed}, WorkspaceRoots: []string{root}})
	got := listWorkspaces(t, httpServer.URL)
	var names []string
	for _, workspace := range got {
		names = append(names, workspace.Name)
	}
	// The named workspace comes first, then the root's directories by name;
	// files and hidden directories are not workspaces.
	if strings.Join(names, ",") != filepath.Base(fixed)+",alpha,beta" {
		t.Fatalf("workspaces = %v", names)
	}
	// /health lists them too, with the default one for older clients.
	_, body := doJSON(t, "GET", httpServer.URL+"/health", "", "")
	if !strings.Contains(string(body), `"workspaces":[`) || !strings.Contains(string(body), `"workspace":"`+fixed+`"`) {
		t.Fatalf("health = %s", body)
	}
}

func TestWorkspaceSetPicksUpNewDirectories(t *testing.T) {
	root := t.TempDir()
	set, err := newWorkspaceSet(nil, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.list()) != 0 {
		t.Fatal("an empty root offered a workspace")
	}
	if err := os.Mkdir(filepath.Join(root, "cloned"), 0o755); err != nil {
		t.Fatal(err)
	}
	set.cachedAt = time.Time{} // past the rescan interval
	if list := set.list(); len(list) != 1 || list[0].Name != "cloned" {
		t.Fatalf("after clone = %+v", list)
	}
	if _, err := newWorkspaceSet(nil, nil); err == nil {
		t.Fatal("a set without workspaces was accepted")
	}
}

func TestSessionsRunInTheirOwnWorkspace(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	var (
		mu  sync.Mutex
		dir string
	)
	launch := func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		mu.Lock()
		dir = spec.Workspace
		mu.Unlock()
		return helperPrepare(t, `{"type":"result","subtype":"success","is_error":false}`, false)(ctx, spec)
	}
	root := t.TempDir()
	httpServer := testServer(t, Config{Token: "secret", Workspaces: []string{first, second}, StoreRoot: root, Launch: launch})
	workspaces := listWorkspaces(t, httpServer.URL)
	if len(workspaces) != 2 {
		t.Fatalf("workspaces = %+v", workspaces)
	}
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"workspace":"`+workspaces[1].ID+`"}`)
	var meta Meta
	_ = json.Unmarshal(body, &meta)
	if status != http.StatusCreated || meta.Workspace != second {
		t.Fatalf("create = %d: %s", status, body)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"hi"}`); status != http.StatusAccepted {
		t.Fatalf("message = %d: %s", status, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := dir
		mu.Unlock()
		if got != "" {
			if got != second {
				t.Fatalf("turn ran in %q, want %q", got, second)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The file API reads the workspace a request names, and only that one.
	if err := os.WriteFile(filepath.Join(second, "only-here.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/files/read?path=only-here.txt&workspace="+workspaces[1].ID, "secret", ""); status != http.StatusOK {
		t.Fatalf("read in its workspace = %d", status)
	}
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/files/read?path=only-here.txt&workspace="+workspaces[0].ID, "secret", ""); status != http.StatusNotFound {
		t.Fatalf("read from the other workspace = %d", status)
	}
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/files/list?workspace=nope", "secret", ""); status != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d", status)
	}
}

func TestConversationOfAnUnservedWorkspaceIsRefused(t *testing.T) {
	root := t.TempDir()
	gone := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(Meta{Title: "old", Workspace: gone})
	if err != nil {
		t.Fatal(err)
	}
	// The server restarted serving another directory only.
	httpServer := testServer(t, Config{Token: "secret", Workspace: t.TempDir(), StoreRoot: root, Launch: helperPrepare(t, "", false)})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+session.ID()+"/messages", "secret", `{"text":"continue"}`)
	if status != http.StatusConflict || !strings.Contains(string(body), "not served") {
		t.Fatalf("message = %d: %s", status, body)
	}
	if events, _ := session.Events(0); len(events) != 0 {
		t.Fatalf("a refused turn stored events: %+v", events)
	}
}
