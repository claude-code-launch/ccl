package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Workspace == "" && len(cfg.Workspaces) == 0 && len(cfg.WorkspaceRoots) == 0 {
		cfg.Workspace = t.TempDir()
	}
	if cfg.StoreRoot == "" {
		cfg.StoreRoot = t.TempDir()
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestNewRequiresTokenBeyondLoopback(t *testing.T) {
	if _, err := New(Config{Token: "", RequireToken: true, Workspace: t.TempDir(), StoreRoot: t.TempDir()}); err == nil {
		t.Fatal("New accepted an exposed listener without a token")
	}
	server, err := New(Config{Workspace: t.TempDir(), StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New in loopback mode: %v", err)
	}
	health := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/health", nil)
	request.Host = "127.0.0.1:8099"
	server.Handler().ServeHTTP(health, request)
	if !strings.Contains(health.Body.String(), `"authRequired":false`) {
		t.Fatalf("/health = %s", health.Body)
	}
}

func TestLoopbackModeNeedsNoToken(t *testing.T) {
	httpServer := testServer(t, Config{})
	for _, path := range []string{"/api/config", "/api/providers", "/api/sessions"} {
		status, body := doJSON(t, "GET", httpServer.URL+path, "", "")
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, status, body)
		}
	}
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "", `{"title":"t"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sessions = %d: %s", status, body)
	}
}

func TestHealthIsPublicAndReportsAuth(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret", Version: "1.2.3"})
	status, body := doJSON(t, "GET", httpServer.URL+"/health", "", "")
	if status != http.StatusOK {
		t.Fatalf("/health = %d: %s", status, body)
	}
	var health struct {
		OK           bool   `json:"ok"`
		Version      string `json:"version"`
		AuthRequired bool   `json:"authRequired"`
		Workspace    string `json:"workspace"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || !health.AuthRequired || health.Version != "1.2.3" || health.Workspace == "" {
		t.Fatalf("health = %+v", health)
	}
}

// builtAssets returns the page and the hashed assets it references.
func builtAssets(t *testing.T, base string) []string {
	t.Helper()
	response, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer response.Body.Close()
	page, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/"}
	for _, match := range regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(string(page), -1) {
		paths = append(paths, match[1])
	}
	if len(paths) < 3 {
		t.Fatalf("index.html references no built script and stylesheet (run npm run build in internal/webui/frontend):\n%s", page)
	}
	return paths
}

func TestStaticAssetsAreServed(t *testing.T) {
	httpServer := testServer(t, Config{})
	for _, path := range builtAssets(t, httpServer.URL) {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, response.StatusCode)
		}
		cache := response.Header.Get("Cache-Control")
		if strings.HasPrefix(path, "/assets/") != strings.Contains(cache, "immutable") {
			t.Fatalf("GET %s Cache-Control = %q", path, cache)
		}
	}
}

func TestStaticAssetsRevalidateByETag(t *testing.T) {
	httpServer := testServer(t, Config{})
	for _, path := range builtAssets(t, httpServer.URL) {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		tag := response.Header.Get("ETag")
		if !strings.HasPrefix(tag, `W/"`) || len(tag) != len(`W/""`)+16 {
			t.Fatalf("GET %s ETag = %q", path, tag)
		}

		request, _ := http.NewRequest("GET", httpServer.URL+path, nil)
		request.Header.Set("If-None-Match", tag)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotModified {
			t.Fatalf("GET %s with a matching ETag = %d, want 304", path, response.StatusCode)
		}

		request.Header.Set("If-None-Match", `W/"stale"`)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s with a stale ETag = %d, want 200", path, response.StatusCode)
		}
	}
}

func TestSessionRoutesRequireToken(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret"})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "", `{"title":"t"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("POST without a token = %d: %s", status, body)
	}
	// The read-only endpoints accept ?token= because EventSource cannot send a
	// header; a wrong token is still refused.
	response, err := http.Get(httpServer.URL + "/api/sessions/nope/events?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("events with a wrong token = %d", response.StatusCode)
	}
}

func TestSessionCRUD(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret"})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"title":"first"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.Title != "first" || meta.Workspace == "" {
		t.Fatalf("created meta = %+v", meta)
	}

	status, body = doJSON(t, "GET", httpServer.URL+"/api/sessions", "secret", "")
	if status != http.StatusOK || !strings.Contains(string(body), meta.ID) {
		t.Fatalf("GET list = %d: %s", status, body)
	}

	status, body = doJSON(t, "PATCH", httpServer.URL+"/api/sessions/"+meta.ID, "secret",
		`{"title":"renamed","provider":"default","effort":"high"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Title != "renamed" || meta.Effort != "high" {
		t.Fatalf("patched meta = %+v", meta)
	}

	// An unknown effort is rejected.
	if status, _ := doJSON(t, "PATCH", httpServer.URL+"/api/sessions/"+meta.ID, "secret", `{"effort":"turbo"}`); status != http.StatusBadRequest {
		t.Fatalf("PATCH bad effort = %d, want 400", status)
	}

	if status, body := doJSON(t, "DELETE", httpServer.URL+"/api/sessions/"+meta.ID, "secret", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}
	if status, _ := doJSON(t, "GET", httpServer.URL+"/api/sessions/"+meta.ID, "secret", ""); status != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404", status)
	}
}

func TestSessionStreamFrames(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret", Launch: helperPrepare(t,
		`{"type":"system","subtype":"init","session_id":"stream-1"}`+"\n"+
			`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1,"output_tokens":2}}`, false)})

	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"title":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	streamURL := httpServer.URL + "/api/sessions/" + meta.ID + "/stream?token=secret"
	request, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q", contentType)
	}

	// Send a message; the stream must deliver its events.
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"hello"}`); status != http.StatusAccepted {
		t.Fatalf("POST message = %d: %s", status, body)
	}

	reader := bufio.NewReader(response.Body)
	var frame strings.Builder
	seen := map[string]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if line == "\n" {
			text := frame.String()
			frame.Reset()
			if strings.Contains(text, "event: user") {
				seen["user"] = true
			}
			for _, kind := range []string{EventInit, EventToken, EventResult} {
				if strings.Contains(text, "event: "+kind) {
					seen[kind] = true
					// Every frame carries its seq so ?since= can resume.
					if !strings.Contains(text, "id: ") {
						t.Fatalf("frame without an id: %q", text)
					}
				}
			}
			if len(seen) >= 3 {
				// The runner still stores the turn after its result; the
				// temporary store is removed only once it is done.
				waitIdle(t, httpServer.URL, meta.ID)
				return
			}
			continue
		}
		frame.WriteString(line)
	}
	t.Fatalf("stream delivered %v", seen)
}

// waitIdle waits until a conversation's turn has finished and been stored.
func waitIdle(t *testing.T, base, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doJSON(t, "GET", base+"/api/sessions/"+id, "secret", "")
		var view struct {
			Running bool `json:"running"`
		}
		if status == http.StatusOK && json.Unmarshal(body, &view) == nil && !view.Running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s still running", id)
}

func TestMessageWhileRunningConflicts(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret", Launch: helperPrepare(t, "", true)})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"title":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"one"}`); status != http.StatusAccepted {
		t.Fatalf("first message = %d: %s", status, body)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"two"}`); status != http.StatusConflict {
		t.Fatalf("second message = %d: %s", status, body)
	}
	// A page loaded mid-turn learns the turn is running from the session itself.
	if running := sessionRunning(t, httpServer.URL, meta.ID); !running {
		t.Fatal("GET session reports running=false during a turn")
	}
	if status, body := doJSON(t, "PATCH", httpServer.URL+"/api/sessions/"+meta.ID, "secret", `{"title":"x"}`); status != http.StatusConflict {
		t.Fatalf("PATCH while running = %d: %s", status, body)
	}
	if status, body := doJSON(t, "DELETE", httpServer.URL+"/api/sessions/"+meta.ID, "secret", ""); status != http.StatusConflict {
		t.Fatalf("DELETE while running = %d: %s", status, body)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/cancel", "secret", ""); status != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s", status, body)
	}

	// The turn finishes once cancelled; the session is usable again.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _ := doJSON(t, "PATCH", httpServer.URL+"/api/sessions/"+meta.ID, "secret", `{"title":"done"}`)
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session stayed busy after cancel")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sessionRunning(t, httpServer.URL, meta.ID) {
		t.Fatal("GET session reports running=true after the turn ended")
	}
}

func sessionRunning(t *testing.T, base, id string) bool {
	t.Helper()
	status, body := doJSON(t, "GET", base+"/api/sessions/"+id, "secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET session = %d: %s", status, body)
	}
	var view struct {
		ID      string `json:"id"`
		Running *bool  `json:"running"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.ID != id || view.Running == nil {
		t.Fatalf("GET session = %s", body)
	}
	return *view.Running
}

func TestEventsSince(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret", Launch: helperPrepare(t,
		`{"type":"result","subtype":"success","is_error":false}`, false)})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"title":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"q"}`); status != http.StatusAccepted {
		t.Fatalf("message = %d: %s", status, body)
	}
	// Wait for the turn to store its result event.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := doJSON(t, "GET", httpServer.URL+"/api/sessions/"+meta.ID+"/events?token=secret", "", "")
		var events []Event
		_ = json.Unmarshal(body, &events)
		if len(events) >= 2 {
			last := events[len(events)-1].Seq
			_, tail := doJSON(t, "GET", httpServer.URL+"/api/sessions/"+meta.ID+"/events?token=secret&since="+itoa(last-1), "", "")
			var resumed []Event
			if err := json.Unmarshal(tail, &resumed); err != nil {
				t.Fatal(err)
			}
			if len(resumed) != 1 || resumed[0].Seq != last {
				t.Fatalf("since=%d returned %+v", last-1, resumed)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn stored %d events", len(events))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWorkspaceIsServerChosen(t *testing.T) {
	workspace := t.TempDir()
	httpServer := testServer(t, Config{Workspace: workspace, StoreRoot: t.TempDir()})
	// A client names a served workspace by id, never a path of its own.
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "", `{"title":"s","workspace":"/etc"}`); status != http.StatusBadRequest {
		t.Fatalf("POST with a path = %d: %s", status, body)
	}
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "", `{"title":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Workspace != workspace {
		t.Fatalf("workspace = %q, want %q", meta.Workspace, workspace)
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func newHTTPTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func TestCloseCancelsTurnsAndRefusesNewOnes(t *testing.T) {
	server, err := New(Config{Token: "secret", Workspace: t.TempDir(), StoreRoot: t.TempDir(), Launch: helperPrepare(t, "", true)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{"title":"s"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var meta Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"one"}`); status != http.StatusAccepted {
		t.Fatalf("message = %d: %s", status, body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sessionRunning(t, httpServer.URL, meta.ID) {
		t.Fatal("a turn is still running after Close")
	}
	// The cancelled turn is stored with its result.
	session, err := server.Store().Open(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1]; last.Type != EventResult {
		t.Fatalf("last event = %s, want a result", last.Type)
	}
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"two"}`); status != http.StatusServiceUnavailable {
		t.Fatalf("message after Close = %d: %s", status, body)
	}
}
