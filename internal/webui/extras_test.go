package webui

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSchedulesSendWhenDueAndSurviveRestart(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Token: "secret", StoreRoot: root, Workspace: t.TempDir(), Launch: helperPrepare(t, `{"type":"result","subtype":"success","is_error":false}`, false)}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, err := server.store.Create(Meta{Title: "s", Workspace: cfg.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := testServerFrom(t, server)
	base := httpServer + "/api/sessions/" + session.ID() + "/schedules"

	if status, body := doJSON(t, "POST", base, "secret", `{"at":"2001-01-01T00:00:00Z","text":"late"}`); status != http.StatusBadRequest {
		t.Fatalf("past time = %d: %s", status, body)
	}
	at := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	status, body := doJSON(t, "POST", base, "secret", `{"at":"`+at+`","text":"good morning"}`)
	if status != http.StatusCreated {
		t.Fatalf("schedule = %d: %s", status, body)
	}
	other := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, body = doJSON(t, "POST", base, "secret", `{"at":"`+other+`","text":"later"}`)
	var later Schedule
	_ = json.Unmarshal(body, &later)

	// A restarted server reads the schedule back from disk.
	if reloaded := newScheduler(schedulePath(root)); len(reloaded.forSession(session.ID())) != 2 {
		t.Fatalf("persisted = %+v", reloaded.items)
	}
	// Not yet due: nothing is sent.
	server.sendDue(time.Now())
	if events, _ := session.Events(0); len(events) != 0 {
		t.Fatalf("sent early: %+v", events)
	}
	server.sendDue(time.Now().Add(3 * time.Second))
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, _ := session.Events(0)
		if len(events) > 0 && strings.Contains(string(events[0].Payload), "good morning") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the due message was not sent: %+v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, body = doJSON(t, "GET", base, "secret", "")
	if strings.Contains(string(body), "good morning") || !strings.Contains(string(body), "later") {
		t.Fatalf("schedules after sending = %s", body)
	}
	if status, _ := doJSON(t, "DELETE", base+"/"+later.ID, "secret", ""); status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
}

func testServerFrom(t *testing.T, server *Server) string {
	t.Helper()
	httpServer := newHTTPTestServer(t, server.Handler())
	return httpServer
}

func TestOAuthLoginRelaysTheURL(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "ccl", `echo "Authenticating $2..."
echo "Open https://auth.example/authorize?state=abc to authorize"
echo "Waiting for callback..."
exit 0
`)
	server, err := New(Config{Token: "secret", Workspace: t.TempDir(), StoreRoot: t.TempDir(), SelfBinary: bin})
	if err != nil {
		t.Fatal(err)
	}
	base := testServerFrom(t, server)
	if status, _ := doJSON(t, "POST", base+"/api/oauth/login", "secret", `{"provider":"evil; rm -rf /"}`); status != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d", status)
	}
	if status, _ := doJSON(t, "POST", base+"/api/oauth/login", "secret", `{"provider":"gpt","alias":"--help"}`); status != http.StatusBadRequest {
		t.Fatalf("option-like alias = %d", status)
	}
	status, body := doJSON(t, "POST", base+"/api/oauth/login", "secret", `{"provider":"gpt","alias":"work"}`)
	if status != http.StatusAccepted {
		t.Fatalf("start = %d: %s", status, body)
	}
	var started struct{ ID string }
	_ = json.Unmarshal(body, &started)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = doJSON(t, "GET", base+"/api/oauth/login/"+started.ID, "secret", "")
		var login oauthLogin
		_ = json.Unmarshal(body, &login)
		if login.Done {
			if !login.OK || len(login.URLs) != 1 || login.URLs[0] != "https://auth.example/authorize?state=abc" {
				t.Fatalf("login = %s", body)
			}
			if !strings.Contains(strings.Join(login.Output, "\n"), "Authenticating gpt") {
				t.Fatalf("output = %v", login.Output)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("login never finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPListHidesSecretsAndRunsClaude(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	workspace := t.TempDir()
	real, _ := filepath.EvalSymlinks(workspace)
	user := map[string]any{
		"mcpServers": map[string]any{"github": map[string]any{"command": "npx", "args": []string{"-y", "gh-mcp"}, "env": map[string]string{"GITHUB_TOKEN": "ghp_secret"}}},
		"projects":   map[string]any{real: map[string]any{"mcpServers": map[string]any{"local-db": map[string]any{"type": "http", "url": "http://127.0.0.1:9000/mcp", "headers": map[string]string{"Authorization": "Bearer hush"}}}}},
	}
	data, _ := json.Marshal(user)
	if err := os.WriteFile(filepath.Join(config, ".claude.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(`{"mcpServers":{"docs":{"type":"sse","url":"https://docs.example/sse"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	writeScript(t, bin, "claude", `printf '%s\n' "$@" > `+argsFile+"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	httpServer := testServer(t, Config{Token: "secret", Workspace: workspace})
	_, body := doJSON(t, "GET", httpServer.URL+"/api/mcp", "secret", "")
	for _, want := range []string{`"name":"github","scope":"user"`, `"name":"local-db","scope":"local"`, `"name":"docs","scope":"project"`, `"env":["GITHUB_TOKEN"]`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("list lacks %s: %s", want, body)
		}
	}
	if strings.Contains(string(body), "ghp_secret") || strings.Contains(string(body), "hush") {
		t.Fatalf("a secret reached the page: %s", body)
	}

	status, body := doJSON(t, "POST", httpServer.URL+"/api/mcp", "secret", `{"name":"fs","scope":"project","type":"stdio","command":"npx","args":["-y","fs-mcp"]}`)
	if status != http.StatusOK {
		t.Fatalf("add = %d: %s", status, body)
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.Join(strings.Fields(string(args)), " "); !strings.HasPrefix(got, "mcp add-json --scope project fs ") || !strings.Contains(got, `"command":"npx"`) {
		t.Fatalf("claude args = %q", got)
	}
	if status, _ := doJSON(t, "POST", httpServer.URL+"/api/mcp", "secret", `{"name":"--evil","scope":"user","command":"x"}`); status != http.StatusBadRequest {
		t.Fatalf("option-like name = %d", status)
	}
	if status, _ := doJSON(t, "DELETE", httpServer.URL+"/api/mcp/fs?scope=project", "secret", ""); status != http.StatusOK {
		t.Fatalf("remove = %d", status)
	}
	args, _ = os.ReadFile(argsFile)
	if got := strings.Join(strings.Fields(string(args)), " "); got != "mcp remove --scope project fs" {
		t.Fatalf("remove args = %q", got)
	}
}

func TestUpdateNotice(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{{"v1.4.0", "v1.3.9", true}, {"v1.3.9", "v1.3.9", false}, {"v1.10.0", "v1.9.0", true}, {"v2.0.0", "dev", false}, {"", "v1.0.0", false}} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Fatalf("newerVersion(%q, %q) = %v", c.latest, c.current, got)
		}
	}
	calls := 0
	httpServer := testServer(t, Config{Token: "secret", Version: "v1.0.0", LatestVersion: func() (string, error) { calls++; return "v1.1.0", nil }})
	for i := 0; i < 3; i++ {
		_, body := doJSON(t, "GET", httpServer.URL+"/api/update", "secret", "")
		if !strings.Contains(string(body), `"newer":true`) {
			t.Fatalf("update = %s", body)
		}
	}
	if calls != 1 {
		t.Fatalf("the release was looked up %d times; it is cached", calls)
	}
}

func TestTerminalRunsAShellOnlyWhenEnabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pty on Windows")
	}
	off := testServer(t, Config{Token: "secret"})
	if status, _ := doJSON(t, "POST", off.URL+"/api/terminal", "secret", `{}`); status != http.StatusForbidden {
		t.Fatalf("terminal without --shell = %d", status)
	}
	t.Setenv("SHELL", "/bin/sh")
	on := testServer(t, Config{Token: "secret", Shell: true})
	status, body := doJSON(t, "POST", on.URL+"/api/terminal", "secret", `{"cols":80,"rows":24}`)
	if status != http.StatusCreated {
		t.Fatalf("open = %d: %s", status, body)
	}
	var term struct{ ID string }
	_ = json.Unmarshal(body, &term)
	defer doJSON(t, "DELETE", on.URL+"/api/terminal/"+term.ID, "secret", "")

	response, err := http.Get(on.URL + "/api/terminal/" + term.ID + "/stream?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if status, body := doJSON(t, "POST", on.URL+"/api/terminal/"+term.ID+"/input", "secret", `{"data":"echo ccl-$((40+2))\n"}`); status != http.StatusNoContent {
		t.Fatalf("input = %d: %s", status, body)
	}
	found := make(chan bool, 1)
	go func() {
		var seen strings.Builder
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var chunk struct{ Data string }
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk)
			decoded, _ := base64.StdEncoding.DecodeString(chunk.Data)
			seen.Write(decoded)
			// The shell computed it: the output is not just the echoed input.
			if strings.Contains(seen.String(), "ccl-42") {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("the stream ended without the command's output")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no output from the shell")
	}
}
