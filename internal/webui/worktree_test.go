package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreeSessionRunsMergesAndCleansUp(t *testing.T) {
	_, workspace := gitWorkspace(t)
	runGit(t, workspace, "checkout", "--quiet", "--", "a.txt")
	if err := os.Remove(filepath.Join(workspace, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "CLAUDE.local.md"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launched := make(chan string, 4)
	prepare := helperPrepare(t, `{"type":"result","subtype":"success","is_error":false}`, false)
	root := t.TempDir()
	base := testServer(t, Config{Token: "secret", Workspace: workspace, StoreRoot: root, Launch: func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		launched <- spec.Workspace
		return prepare(ctx, spec)
	}}).URL

	status, body := doJSON(t, "POST", base+"/api/sessions", "secret", `{"worktree":true}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	var session SessionView
	_ = json.Unmarshal(body, &session)
	tree := session.Worktree
	if tree == nil || tree.Branch != "ccl/"+session.ID[:8] || tree.Base != "main" || session.Workspace != workspace {
		t.Fatalf("session = %s", body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, ".worktrees", "*", tree.Name))
	if len(matches) != 1 {
		t.Fatalf("worktree dirs = %v", matches)
	}
	dir := matches[0]
	if data, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.local.md")); string(data) != "mine\n" {
		t.Fatalf("CLAUDE.local.md was not copied: %q", data)
	}
	// The project's own status stays clean.
	if out := runGit(t, workspace, "status", "--porcelain", "--untracked-files=no"); out != "" {
		t.Fatalf("project status = %q", out)
	}

	// A turn runs in the worktree.
	if status, body := doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/messages", "secret", `{"text":"hi"}`); status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("message = %d %s", status, body)
	}
	select {
	case got := <-launched:
		if got != dir {
			t.Fatalf("turn ran in %s, want %s", got, dir)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not launch")
	}
	waitIdle(t, base, session.ID)

	// Files and git requests reach the worktree with &worktree=.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	_, body = doJSON(t, "GET", base+"/api/workspaces", "secret", "")
	_ = json.Unmarshal(body, &list)
	query := "?workspace=" + list.Workspaces[0].ID + "&worktree=" + tree.Name
	if status, body := doJSON(t, "GET", base+"/api/files/read"+query+"&path=b.txt", "secret", ""); status != http.StatusOK || !strings.Contains(string(body), "branch") {
		t.Fatalf("worktree read = %d %s", status, body)
	}
	if status, _ := doJSON(t, "GET", base+"/api/files/read?workspace="+list.Workspaces[0].ID+"&path=b.txt", "secret", ""); status != http.StatusNotFound {
		t.Fatalf("project read of a worktree file = %d", status)
	}
	if status, _ := doJSON(t, "GET", base+"/api/files/read?workspace="+list.Workspaces[0].ID+"&worktree=../x&path=b.txt", "secret", ""); status != http.StatusNotFound {
		t.Fatalf("bad worktree name = %d", status)
	}

	// Uncommitted work refuses a merge and a delete.
	status, body = doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/worktree/merge", "secret", `{}`)
	if status != http.StatusConflict || !strings.Contains(string(body), "uncommitted") {
		t.Fatalf("dirty merge = %d %s", status, body)
	}
	if status, _ := doJSON(t, "DELETE", base+"/api/sessions/"+session.ID, "secret", ""); status != http.StatusConflict {
		t.Fatalf("dirty delete = %d", status)
	}

	runGit(t, dir, "add", "b.txt")
	runGit(t, dir, "commit", "--quiet", "-m", "on the branch")
	var wt WorktreeStatus
	_, body = doJSON(t, "GET", base+"/api/sessions/"+session.ID+"/worktree", "secret", "")
	_ = json.Unmarshal(body, &wt)
	if !wt.Exists || wt.Ahead != 1 || wt.Dirty != 0 || len(wt.Files) != 1 || wt.Files[0].Path != "b.txt" || wt.Current != "main" {
		t.Fatalf("status = %s", body)
	}
	if status, body = doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/worktree/merge", "secret", `{}`); status != http.StatusOK {
		t.Fatalf("merge = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(workspace, "b.txt")); err != nil {
		t.Fatalf("merge did not reach the project: %v", err)
	}
	if status, body = doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/worktree/push", "secret", `{}`); status != http.StatusConflict || !strings.Contains(string(body), "no remote") {
		t.Fatalf("push without a remote = %d %s", status, body)
	}

	// Deleting the conversation removes the worktree and the merged branch.
	if status, body = doJSON(t, "DELETE", base+"/api/sessions/"+session.ID, "secret", ""); status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", status, body)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := runGit(t, workspace, "branch", "--list", tree.Branch); out != "" {
		t.Fatalf("merged branch kept: %q", out)
	}
}

func TestWorktreeNeedsARepository(t *testing.T) {
	base := testServer(t, Config{Token: "secret"}).URL
	status, body := doJSON(t, "POST", base+"/api/sessions", "secret", `{"worktree":true}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "not a git repository") {
		t.Fatalf("create = %d %s", status, body)
	}
}
