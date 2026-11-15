package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func filesServer(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("README.md", "# hi\n")
	mustWrite("src/main.go", "package main\n")
	mustWrite("src/util/helper.go", "package util\n")
	mustWrite(".git/config", "[core]\n")
	mustWrite("bin.dat", "a\x00b")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A link that leads out of the workspace must not be followed.
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	httpServer := testServer(t, Config{Token: "secret", Workspace: workspace})
	return httpServer.URL, workspace
}

func TestFilesListHidesGitAndSortsDirsFirst(t *testing.T) {
	base, _ := filesServer(t)
	status, body := doJSON(t, "GET", base+"/api/files/list?path=", "secret", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d: %s", status, body)
	}
	var listing struct {
		Entries []FileEntry `json:"entries"`
	}
	_ = json.Unmarshal(body, &listing)
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name+":"+entry.Type)
	}
	joined := strings.Join(names, ",")
	if strings.Contains(joined, ".git") || !strings.HasPrefix(joined, "src:dir") {
		t.Fatalf("entries = %s", joined)
	}
}

func TestFilesStayInsideWorkspace(t *testing.T) {
	base, _ := filesServer(t)
	for _, rel := range []string{"../../etc/passwd", "/etc/passwd", "escape/secret.txt", "src/../../x"} {
		status, body := doJSON(t, "GET", base+"/api/files/read?path="+url.QueryEscape(rel), "secret", "")
		// /etc/passwd and ../ clean to paths inside the workspace that do not
		// exist; the link is refused outright. Neither reads the real file.
		if status == http.StatusOK || strings.Contains(string(body), "secret\"") || strings.Contains(string(body), "root:") {
			t.Fatalf("read %q = %d: %s", rel, status, body)
		}
	}
	if status, _ := doJSON(t, "GET", base+"/api/files/read?path=escape/secret.txt", "secret", ""); status != http.StatusForbidden {
		t.Fatalf("symlink escape = %d, want 403", status)
	}
	if status, _ := doJSON(t, "PUT", base+"/api/files/write", "secret", `{"path":"escape/new.txt","content":"x"}`); status != http.StatusForbidden {
		t.Fatalf("write through escaping link = %d, want 403", status)
	}
}

func TestFilesReadWriteWithConflictCheck(t *testing.T) {
	base, workspace := filesServer(t)
	status, body := doJSON(t, "GET", base+"/api/files/read?path=src/main.go", "secret", "")
	var file FileContent
	_ = json.Unmarshal(body, &file)
	if status != http.StatusOK || file.Content != "package main\n" || file.Hash == "" || file.Binary {
		t.Fatalf("read = %d: %s", status, body)
	}
	save := func(content, hash string) (int, []byte) {
		payload, _ := json.Marshal(map[string]string{"path": "src/main.go", "content": content, "expectedHash": hash})
		return doJSON(t, "PUT", base+"/api/files/write", "secret", string(payload))
	}
	if status, body := save("package main // edited\n", file.Hash); status != http.StatusOK {
		t.Fatalf("save = %d: %s", status, body)
	}
	// Claude changed the file meanwhile: the stale hash is refused.
	if status, body := save("package main // stale\n", file.Hash); status != http.StatusConflict {
		t.Fatalf("stale save = %d: %s", status, body)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "src", "main.go"))
	if string(data) != "package main // edited\n" {
		t.Fatalf("file = %q", data)
	}
	// A new file needs no hash, and an existing one cannot be overwritten blind.
	if status, body := doJSON(t, "PUT", base+"/api/files/write", "secret", `{"path":"docs/new.md","content":"new"}`); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, body)
	}
	if status, _ := doJSON(t, "PUT", base+"/api/files/write", "secret", `{"path":"README.md","content":"x"}`); status != http.StatusConflict {
		t.Fatalf("blind overwrite = %d, want 409", status)
	}
	status, body = doJSON(t, "GET", base+"/api/files/read?path=bin.dat", "secret", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"binary":true`) || strings.Contains(string(body), `"content"`) {
		t.Fatalf("binary read = %d: %s", status, body)
	}
}

func TestFilesSearchRanksNameMatches(t *testing.T) {
	base, _ := filesServer(t)
	status, body := doJSON(t, "GET", base+"/api/files/search?q=help", "secret", "")
	var result struct {
		Files []string `json:"files"`
	}
	_ = json.Unmarshal(body, &result)
	if status != http.StatusOK || len(result.Files) == 0 || result.Files[0] != "src/util/helper.go" {
		t.Fatalf("search = %d: %s", status, body)
	}
	_, body = doJSON(t, "GET", base+"/api/files/search?q=config", "secret", "")
	if strings.Contains(string(body), ".git/") {
		t.Fatalf("search reached .git: %s", body)
	}
}

func TestFilesRawNeedsTokenAndServesSafely(t *testing.T) {
	base, _ := filesServer(t)
	response, err := http.Get(base + "/api/files/raw?path=README.md")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("raw without token = %d", response.StatusCode)
	}
	response, err = http.Get(base + "/api/files/raw?path=README.md&token=secret")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("raw = %d %q", response.StatusCode, response.Header.Get("Content-Disposition"))
	}
}

func TestFilesUploadIntoWorkspaceNeverOverwrites(t *testing.T) {
	base, workspace := filesServer(t)
	status, _, raw := uploadTo(t, base+"/api/files/uploads?dir=src", map[string][]byte{"notes.txt": []byte("hello")})
	if status != http.StatusCreated || !strings.Contains(raw, "src/notes.txt") {
		t.Fatalf("upload = %d: %s", status, raw)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "src", "notes.txt")); string(data) != "hello" {
		t.Fatalf("file = %q", data)
	}
	if status, _, raw := uploadTo(t, base+"/api/files/uploads?dir=src", map[string][]byte{"main.go": []byte("x")}); status != http.StatusConflict {
		t.Fatalf("overwrite = %d: %s", status, raw)
	}
	if status, _, _ := uploadTo(t, base+"/api/files/uploads?dir=escape", map[string][]byte{"x.txt": []byte("x")}); status != http.StatusForbidden {
		t.Fatalf("upload through escaping link = %d", status)
	}
}

func uploadTo(t *testing.T, url string, files map[string][]byte) (int, []Attachment, string) {
	t.Helper()
	return upload(t, url, files)
}
