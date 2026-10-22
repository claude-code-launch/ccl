package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitWorkspace is a repository with one commit, one modified file and one
// untracked file. Global git configuration is replaced by the test's own.
func gitWorkspace(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	config := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(config, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	workspace := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "--quiet")
	write("a.txt", "one\ntwo\n")
	run("add", "a.txt")
	run("commit", "--quiet", "-m", "first")
	write("a.txt", "one\nTWO\nthree\n")
	write("new.txt", "x\ny\n")
	httpServer := testServer(t, Config{Token: "secret", Workspace: workspace})
	return httpServer.URL, workspace
}

func gitStatusOf(t *testing.T, body []byte) GitStatus {
	t.Helper()
	var status GitStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("status %s: %v", body, err)
	}
	return status
}

func TestGitStatusDiffStageCommit(t *testing.T) {
	base, _ := gitWorkspace(t)
	status, body := doJSON(t, "GET", base+"/api/git/status", "secret", "")
	st := gitStatusOf(t, body)
	if status != http.StatusOK || !st.Repo || st.Branch != "main" || len(st.Files) != 2 {
		t.Fatalf("status = %d: %s", status, body)
	}
	// a.txt: one line changed, one added; new.txt: two new lines.
	if st.Added != 4 || st.Deleted != 1 {
		t.Fatalf("totals +%d -%d, want +4 -1: %s", st.Added, st.Deleted, body)
	}

	_, body = doJSON(t, "GET", base+"/api/git/diff?path=a.txt", "secret", "")
	if !strings.Contains(string(body), `+TWO`) {
		t.Fatalf("diff = %s", body)
	}
	_, body = doJSON(t, "GET", base+"/api/git/diff?path=new.txt&untracked=1", "secret", "")
	if !strings.Contains(string(body), `+x`) {
		t.Fatalf("untracked diff = %s", body)
	}

	if status, body := doJSON(t, "POST", base+"/api/git/stage", "secret", `{"paths":["a.txt","new.txt"]}`); status != http.StatusOK {
		t.Fatalf("stage = %d: %s", status, body)
	}
	if status, body := doJSON(t, "POST", base+"/api/git/unstage", "secret", `{"paths":["new.txt"]}`); status != http.StatusOK || !strings.Contains(string(body), `"untracked":true`) {
		t.Fatalf("unstage = %d: %s", status, body)
	}
	// The message is data on stdin: an option-looking message is just text.
	if status, body := doJSON(t, "POST", base+"/api/git/commit", "secret", `{"message":"--amend edit a"}`); status != http.StatusOK {
		t.Fatalf("commit = %d: %s", status, body)
	}
	_, body = doJSON(t, "GET", base+"/api/git/log", "secret", "")
	if !strings.Contains(string(body), `"subject":"--amend edit a"`) || !strings.Contains(string(body), `"subject":"first"`) {
		t.Fatalf("log = %s", body)
	}
	if status, _ := doJSON(t, "POST", base+"/api/git/commit", "secret", `{"message":"  "}`); status != http.StatusBadRequest {
		t.Fatalf("empty message = %d", status)
	}
}

func TestGitBranchesAndCheckout(t *testing.T) {
	base, _ := gitWorkspace(t)
	if status, body := doJSON(t, "POST", base+"/api/git/checkout", "secret", `{"branch":"feature/x","create":true}`); status != http.StatusOK {
		t.Fatalf("create branch = %d: %s", status, body)
	}
	_, body := doJSON(t, "GET", base+"/api/git/branches", "secret", "")
	if !strings.Contains(string(body), `"name":"feature/x","current":true`) {
		t.Fatalf("branches = %s", body)
	}
	for _, bad := range []string{"-f", "a..b", "", "--orphan=x"} {
		payload, _ := json.Marshal(map[string]any{"branch": bad})
		if status, _ := doJSON(t, "POST", base+"/api/git/checkout", "secret", string(payload)); status != http.StatusBadRequest {
			t.Fatalf("branch %q = %d, want 400", bad, status)
		}
	}
	if status, _ := doJSON(t, "GET", base+"/api/git/show?commit=--help", "secret", ""); status != http.StatusBadRequest {
		t.Fatalf("show option = %d", status)
	}
}

func TestGitPathsStayInsideWorkspace(t *testing.T) {
	base, _ := gitWorkspace(t)
	outside := filepath.Join(t.TempDir(), "x")
	payload, _ := json.Marshal(map[string]any{"paths": []string{"../" + filepath.Base(outside)}})
	// ../x cleans to x inside the workspace, which does not exist: git refuses.
	if status, _ := doJSON(t, "POST", base+"/api/git/stage", "secret", string(payload)); status == http.StatusOK {
		t.Fatal("staged a path outside the workspace")
	}
}

func TestGitStatusOutsideRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	httpServer := testServer(t, Config{Token: "secret"})
	_, body := doJSON(t, "GET", httpServer.URL+"/api/git/status", "secret", "")
	if st := gitStatusOf(t, body); st.Repo || st.Files == nil {
		t.Fatalf("status = %s", body)
	}
}
