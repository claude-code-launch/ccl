package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestMain(m *testing.M) {
	if os.Getenv("CCL_WEBUI_FIXTURE") == "1" && len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(fakeGrok())
	}
	os.Exit(m.Run())
}

func TestBrowserSessionLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CCL_WEBUI_FIXTURE", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	grok, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	workspace, err = resolveWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		GrokPath: grok, StateDir: filepath.Join(home, "state"), Workspaces: []string{workspace},
		Provider: provider.Provider{
			Name: "fixture", Type: "openai_responses", Model: "fixture",
			Endpoint: "http://127.0.0.1:9/v1", APIKey: "fake-upstream-key",
		},
	}
	first, stop := testServer(t, opts)
	first.check("GET", "sessions", nil, "", "", "", http.StatusUnauthorized)
	first.check("GET", "sessions", nil, first.Fragment, "", "rebind.example", http.StatusForbidden)
	first.check("POST", "sessions", map[string]string{"workspace": workspace}, first.Fragment,
		"http://127.0.0.1:9", "", http.StatusForbidden)
	first.check("POST", "sessions", map[string]string{"workspace": home}, first.Fragment, "", "", http.StatusBadRequest)
	created := first.request("POST", "sessions", map[string]string{"workspace": workspace}, http.StatusCreated)
	var chat summary
	if err := json.Unmarshal(created, &chat); err != nil || chat.ID == "" {
		t.Fatalf("could not decode session: %v", err)
	}
	base := "sessions/" + chat.ID
	stream := first.events(base)
	stream.find("state", "idle")

	first.request("POST", base+"/prompt", map[string]string{"text": "first turn"}, http.StatusAccepted)
	stream.find("assistant", "reply: first turn")
	stream.find("state", "idle")

	first.request("POST", base+"/prompt", map[string]string{"text": "permission"}, http.StatusAccepted)
	permit := stream.find("permission", "Fixture command")
	if !permit.CanAllow || !strings.Contains(permit.Detail, "fixture-command") {
		t.Fatalf("incomplete permission event: %+v", permit)
	}
	first.request("POST", base+"/prompt", map[string]string{"text": "overlap"}, http.StatusConflict)
	decision := base + "/permissions/" + permit.ID
	first.request("POST", decision, map[string]string{"decision": "deny"}, http.StatusNoContent)
	first.request("POST", decision, map[string]string{"decision": "allow"}, http.StatusConflict)
	stream.find("assistant", "decision: reject-one")
	stream.find("state", "idle")

	first.request("POST", base+"/prompt", map[string]string{"text": "permission"}, http.StatusAccepted)
	permit = stream.find("permission", "Fixture command")
	first.request("POST", base+"/permissions/"+permit.ID, map[string]string{"decision": "allow"}, http.StatusNoContent)
	stream.find("assistant", "decision: allow-one")
	stream.find("state", "idle")

	first.request("POST", base+"/prompt", map[string]string{"text": "permission"}, http.StatusAccepted)
	permit = stream.find("permission", "Fixture command")
	first.request("POST", base+"/cancel", map[string]string{}, http.StatusNoContent)
	stream.find("permission_closed", "")
	stream.find("state", "idle")
	first.request("POST", base+"/permissions/"+permit.ID, map[string]string{"decision": "allow"}, http.StatusConflict)

	first.request("POST", base+"/prompt", map[string]string{"text": "wait for cancel"}, http.StatusAccepted)
	stream.find("assistant", "ready to cancel")
	first.request("POST", base+"/cancel", map[string]string{}, http.StatusNoContent)
	stream.find("assistant", "ACP cancellation received")
	stream.find("state", "idle")
	stream.close()
	stop()

	second, _ := testServer(t, opts)
	second.check("GET", "sessions", nil, first.Fragment, "", "", http.StatusUnauthorized)
	stream = second.events(base)
	stream.find("user", "first turn")
	stream.find("assistant", "reply: first turn")
	stream.find("state", "idle")
	second.request("POST", base+"/prompt", map[string]string{"text": "continued"}, http.StatusAccepted)
	stream.find("assistant", "reply: continued")
	stream.find("state", "idle")
	stream.close()

	data := second.request("GET", "sessions", nil, http.StatusOK)
	if bytes.Contains(data, []byte("fixture-agent-id")) || bytes.Contains(data, []byte("fake-upstream-key")) {
		t.Fatalf("session list exposed private worker data")
	}
}

type browserTest struct {
	*url.URL
	t *testing.T
}

func testServer(t *testing.T, opts Options) (browserTest, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Serve(ctx, opts, func(address string) { ready <- address }); err != nil {
			t.Error(err)
		}
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	}
	t.Cleanup(stop)
	select {
	case address := <-ready:
		base, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		return browserTest{base, t}, stop
	case <-done:
		t.Fatal("server exited before readiness")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return browserTest{}, stop
}

func (b browserTest) send(method, path string, body any, token, origin, host string) (*http.Response, context.CancelFunc) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(b.t.Context(), 20*time.Second)
	b.t.Cleanup(cancel)
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+b.Host+"/api/"+path, reader)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	return resp, cancel
}

func (b browserTest) request(method, path string, body any, code int) []byte {
	b.t.Helper()
	return b.check(method, path, body, b.Fragment, "", "", code)
}

func (b browserTest) check(method, path string, body any, token, origin, host string, code int) []byte {
	b.t.Helper()
	resp, cancel := b.send(method, path, body, token, origin, host)
	defer cancel()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != code {
		b.t.Fatalf("%s %s = %d, wanted %d: %q (%v)", method, path, resp.StatusCode, code, data, err)
	}
	return data
}

type testStream struct {
	t       *testing.T
	scanner *bufio.Scanner
	batch   []event
	close   func()
}

func (b browserTest) events(path string) *testStream {
	b.t.Helper()
	resp, cancel := b.send("GET", path+"/events", nil, b.Fragment, "", "")
	closeStream := func() { cancel(); resp.Body.Close() }
	b.t.Cleanup(closeStream)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		b.t.Fatalf("stream rejected: %s", resp.Status)
	}
	return &testStream{t: b.t, scanner: bufio.NewScanner(resp.Body), close: closeStream}
}

func (s *testStream) find(kind, text string) event {
	s.t.Helper()
	for {
		for len(s.batch) > 0 {
			e := s.batch[0]
			s.batch = s.batch[1:]
			if e.Kind == kind && (text == "" || e.Text == text) {
				return e
			}
		}
		if !s.scanner.Scan() {
			s.t.Fatalf("no event %s %q: %v", kind, text, s.scanner.Err())
		}
		if raw, ok := strings.CutPrefix(s.scanner.Text(), "data: "); ok {
			if err := json.Unmarshal([]byte(raw), &s.batch); err != nil {
				s.t.Fatal(err)
			}
		}
	}
}

func fakeGrok() int {
	const id = "fixture-agent-id"
	profile := os.Getenv("GROK_HOME")
	if profile == "" || os.Getenv("XAI_API_KEY") == "fake-upstream-key" ||
		!strings.HasPrefix(os.Getenv("GROK_MODELS_BASE_URL"), "http://127.0.0.1:") {
		return 2
	}
	history := filepath.Join(profile, "turns")
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	send := func(fields map[string]any) {
		fields["jsonrpc"] = "2.0"
		_ = encoder.Encode(fields)
	}
	chunk := func(role, text string) {
		send(map[string]any{
			"method": "session/update", "params": map[string]any{
				"sessionId": id, "update": map[string]any{
					"sessionUpdate": role + "_message_chunk", "content": map[string]string{"type": "text", "text": text},
				},
			},
		})
	}
	for {
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Prompt []struct{ Text string }
			}
		}
		if decoder.Decode(&msg) != nil {
			return 0
		}
		result := make(map[string]any)
		switch msg.Method {
		case "initialize":
			result["protocolVersion"] = 1
			result["agentCapabilities"] = map[string]bool{"loadSession": true}
		case "session/new":
			result["sessionId"] = id
		case "session/load":
			if data, err := os.ReadFile(history); err == nil {
				for _, text := range strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' }) {
					chunk("user", text)
					chunk("agent", "reply: "+text)
				}
			}
		case "session/prompt":
			text := msg.Params.Prompt[0].Text
			result["stopReason"] = "end_turn"
			switch text {
			case "permission":
				send(map[string]any{
					"id": 9000, "method": "session/request_permission", "params": map[string]any{
						"sessionId": id, "toolCall": map[string]any{
							"toolCallId": "tool-id", "title": "Fixture command", "rawInput": map[string]string{"command": "fixture-command"},
						},
						"options": []map[string]string{
							{"optionId": "allow-all", "kind": "allow_always", "name": "Always allow"},
							{"optionId": "allow-one", "kind": "allow_once", "name": "Allow once"},
							{"optionId": "reject-one", "kind": "reject_once", "name": "Deny once"},
						},
					},
				})
				for {
					var reply struct {
						ID     int
						Result struct {
							Outcome struct{ Outcome, OptionID string }
						}
					}
					if decoder.Decode(&reply) != nil {
						return 3
					}
					if reply.ID == 9000 {
						chunk("agent", "decision: "+reply.Result.Outcome.OptionID)
						break
					}
				}
			case "wait for cancel":
				chunk("agent", "ready to cancel")
				for {
					var notification struct{ Method string }
					if decoder.Decode(&notification) != nil {
						return 3
					}
					if notification.Method == "session/cancel" {
						chunk("agent", "ACP cancellation received")
						break
					}
				}
				result["stopReason"] = "cancelled"
			default:
				old, _ := os.ReadFile(history)
				if os.WriteFile(history, append(old, []byte(text+"\n")...), 0o600) != nil {
					return 4
				}
				chunk("agent", "reply: "+text)
			}
		default:
			continue
		}
		send(map[string]any{"id": msg.ID, "result": result})
	}
}
