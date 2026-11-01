package oauthproxy

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cursorStubUpstream serves the Connect Run path from a scripted handler and
// records the frames CCL writes there, so a test can assert both directions.
type cursorStubUpstream struct {
	server *httptest.Server
	calls  int
	// run handles Run; a nil run answers 200 with an immediate end-stream.
	run func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame)
}

type cursorStubFrame struct {
	Flags byte
	Body  []byte
}

// start points the runtime at the stub for the duration of the test.
func newCursorStubUpstream(t *testing.T, run func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame)) *cursorStubUpstream {
	t.Helper()
	stub := &cursorStubUpstream{run: run}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stub.calls++
		if stub.run == nil || request.URL.Path != cursorConnectRunPath {
			writer.WriteHeader(http.StatusOK)
			return
		}
		// Run is a bidirectional stream: the answer must be readable while the
		// request body is still being written.
		_ = http.NewResponseController(writer).EnableFullDuplex()
		frames := make(chan cursorStubFrame, 32)
		reader := bufio.NewReader(request.Body)
		go func() {
			defer close(frames)
			for {
				flags, payload, err := cursorReadFrame(reader)
				if err != nil {
					return
				}
				frames <- cursorStubFrame{Flags: flags, Body: payload}
			}
		}()
		stub.run(writer, request, frames)
	}))
	old := cursorConnectBaseURL
	cursorConnectBaseURL = stub.server.URL
	t.Cleanup(func() {
		cursorConnectBaseURL = old
		stub.server.Close()
	})
	return stub
}

// writeCursorFrame writes one Connect frame and flushes it.
func writeCursorFrame(writer http.ResponseWriter, payload []byte) {
	_, _ = writer.Write(cursorFrameConnect(payload, 0))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// cursorTestInteraction builds one AgentServerMessage carrying an interaction
// update with the given inner field number and text.
func cursorTestInteraction(field int, text string) []byte {
	inner := cursorAppendString(nil, cursorTDUText, text)
	return cursorAppendBytes(nil, cursorASMInteractionUpdate, cursorAppendBytes(nil, field, inner))
}

func cursorTestTurnEnd() []byte {
	return cursorAppendBytes(nil, cursorASMInteractionUpdate, cursorAppendBytes(nil, cursorIUTurnEnded, nil))
}

// cursorTestExecMCP builds an AgentServerMessage asking the client to run one
// MCP tool.
func cursorTestExecMCP(msgID uint32, execID, name, callID string, args map[string]string) []byte {
	mcpArgs := cursorAppendString(nil, cursorMCAName, name)
	mcpArgs = cursorAppendString(mcpArgs, cursorMCAToolCallID, callID)
	for key, value := range args {
		// Map values are protobuf Value messages, exactly as Cursor sends them.
		quoted, _ := json.Marshal(value)
		entry := cursorAppendString(nil, 1, key)
		entry = cursorAppendBytes(entry, 2, cursorJSONToProtobufValue(quoted))
		mcpArgs = cursorAppendBytes(mcpArgs, cursorMCAArgs, entry)
	}
	exec := cursorAppendUint32(nil, cursorESMID, msgID)
	exec = cursorAppendStringRaw(exec, cursorESMExecID, execID)
	exec = cursorAppendBytes(exec, cursorESMMcpArgs, mcpArgs)
	return cursorAppendBytes(nil, cursorASMExecServer, exec)
}

func cursorTestCredential(t *testing.T, token string) *cursorOAuthAuthorizer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor-abcd.json")
	metadata := map[string]any{
		"type":           ProviderCursor,
		"access_token":   token,
		"refresh_token":  "refresh-token",
		"email":          "dev@example.com",
		"last_refresh":   time.Now().UTC().Format(time.RFC3339),
		"token_obtained": time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return &cursorOAuthAuthorizer{path: path, client: http.DefaultClient}
}

func newCursorTestService(t *testing.T, authorizer *cursorOAuthAuthorizer) *cursorService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &cursorService{
		apiKey:     "local-key",
		models:     []string{"composer-2"},
		modelRoute: map[string]string{"composer-2": "composer-2"},
		authorizer: authorizer,
		client:     &http.Client{Transport: newUpstreamTransport(30 * time.Second)},
		ctx:        ctx,
	}
}

func cursorTestStreamRequest(t *testing.T, service *cursorService) *http.Request {
	t.Helper()
	payload := `{
		"model":"composer-2",
		"max_tokens":1024,
		"stream":true,
		"messages":[{"role":"user","content":"hello there"}]
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("x-api-key", service.apiKey)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func cursorTestToolResultRequest(t *testing.T, service *cursorService, callID, result string) *http.Request {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model":      "composer-2",
		"max_tokens": 1024,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "user", "content": "hello there"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": callID, "name": "Read", "input": map[string]any{"path": "/tmp/a"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": callID, "content": result},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(payload)))
	request.Header.Set("x-api-key", service.apiKey)
	request.Header.Set("Content-Type", "application/json")
	return request
}

// serveCursorTurn runs one request through the runtime and returns the status
// and body.
func serveCursorTurn(t *testing.T, service *cursorService, request *http.Request) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	service.handler().ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestCursorStreamEmitsTextThenParksOnMcpCall(t *testing.T) {
	var sawAuthorization string
	stub := newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		sawAuthorization = request.Header.Get("Authorization")
		if got := request.Header.Get("Content-Type"); got != "application/connect+proto" {
			t.Errorf("content-type = %q", got)
		}
		if got := request.Header.Get("X-Cursor-Client-Version"); got != cursorCLIVersion {
			t.Errorf("client version = %q", got)
		}
		if got := request.Header.Get("User-Agent"); !strings.HasPrefix(got, "ccl/") {
			t.Errorf("user agent = %q", got)
		}
		// The Run frame arrives before any answer.
		select {
		case frame := <-frames:
			if frame.Flags != 0 {
				t.Errorf("run frame flags = %d", frame.Flags)
			}
		case <-time.After(5 * time.Second):
			t.Error("no Run frame")
			return
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "Hi "))
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "there"))
		writeCursorFrame(writer, cursorTestExecMCP(1, "exec-1", "Read", "toolu_1", map[string]string{"path": "/tmp/a"}))
		// A parked stream stays open; keep the handler alive until the client
		// goes away.
		<-request.Context().Done()
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
	defer service.closeParked()

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if sawAuthorization != "Bearer access-token" {
		t.Fatalf("authorization = %q", sawAuthorization)
	}
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta","text":"Hi "`,
		`"type":"text_delta","text":"there"`,
		`"type":"tool_use","id":"toolu_1","name":"Read"`,
		`"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a\"}"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "event: content_block_start") {
		t.Fatalf("stream has no content block:\n%s", body)
	}
	service.mu.Lock()
	parked := service.parked
	service.mu.Unlock()
	if parked == nil {
		t.Fatal("the stream was not parked for the tool result")
	}
	if !parked.matches([]string{"toolu_1"}) {
		t.Fatal("parked stream does not own the tool call it reported")
	}
	if stub.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", stub.calls)
	}
}

func TestCursorResumesParkedStreamWithToolResult(t *testing.T) {
	received := make(chan string, 1)
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		<-frames // the Run frame
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, cursorTestExecMCP(1, "exec-1", "Read", "toolu_1", map[string]string{"path": "/tmp/a"}))
		select {
		case frame := <-frames:
			received <- describeCursorFrame(frame)
		case <-time.After(5 * time.Second):
			received <- "timeout"
			return
		}
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "done"))
		writeCursorFrame(writer, cursorTestTurnEnd())
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
	defer service.closeParked()

	if status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service)); status != http.StatusOK {
		t.Fatalf("park status = %d body=%s", status, body)
	}
	// The result request must continue the parked stream, not open a new Run.
	status, body := serveCursorTurn(t, service, cursorTestToolResultRequest(t, service, "toolu_1", "file contents"))
	if status != http.StatusOK {
		t.Fatalf("resume status = %d body=%s", status, body)
	}
	if !strings.Contains(body, `"type":"text_delta","text":"done"`) {
		t.Fatalf("resume stream missing the continued text:\n%s", body)
	}
	select {
	case frame := <-received:
		if !strings.Contains(frame, "exec-1") || !strings.Contains(frame, "file contents") {
			t.Fatalf("tool result frame = %s", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool result never reached the upstream stream")
	}
	service.mu.Lock()
	parked := service.parked
	service.mu.Unlock()
	if parked != nil {
		t.Fatal("a completed stream stayed parked")
	}
}

// describeCursorFrame renders a client frame as its decodable text, so a test
// can assert what CCL sent without hard-coding protobuf bytes.
func describeCursorFrame(frame cursorStubFrame) string {
	var out strings.Builder
	walkCursorStrings(frame.Body, &out)
	return out.String()
}

func walkCursorStrings(data []byte, out *strings.Builder) {
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			return
		}
		data = data[n:]
		switch typ {
		case cursorWireBytes:
			value, n := cursorConsumeBytes(data)
			if n < 0 {
				return
			}
			data = data[n:]
			if isCursorReadableText(value) {
				out.WriteString(string(value))
				out.WriteByte(' ')
			}
			walkCursorStrings(value, out)
		case cursorWireVarint, cursorWireFixed64, cursorWireFixed32:
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return
			}
			data = data[skip:]
		default:
			_ = num
			return
		}
	}
}

func isCursorReadableText(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

func TestCursorQueuesExtraMcpCallsUntilTheirResults(t *testing.T) {
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		<-frames
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, cursorTestExecMCP(1, "exec-1", "Read", "toolu_1", map[string]string{"path": "/tmp/a"}))
		writeCursorFrame(writer, cursorTestExecMCP(2, "exec-2", "Grep", "toolu_2", map[string]string{"pattern": "x"}))
		<-request.Context().Done()
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
	defer service.closeParked()

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	// One Anthropic response carries one tool call; the second stays queued on
	// the parked stream and is surfaced by the turn that resumes it.
	if strings.Contains(body, "toolu_2") {
		t.Fatalf("the second call leaked into the first response:\n%s", body)
	}
	service.mu.Lock()
	parked := service.parked
	service.mu.Unlock()
	if parked == nil || !parked.matches([]string{"toolu_1"}) {
		t.Fatal("the first call must stay pending on the parked stream")
	}

	status, body = serveCursorTurn(t, service, cursorTestToolResultRequest(t, service, "toolu_1", "first"))
	if status != http.StatusOK {
		t.Fatalf("first result status = %d body=%s", status, body)
	}
	if !strings.Contains(body, "toolu_2") {
		t.Fatalf("the queued call was not surfaced on resume:\n%s", body)
	}
	service.mu.Lock()
	parked = service.parked
	service.mu.Unlock()
	if parked == nil || !parked.matches([]string{"toolu_2"}) {
		t.Fatal("the queued call must stay answerable")
	}
}

func TestCursorRejectsBuiltinToolsImmediately(t *testing.T) {
	exec := cursorAppendUint32(nil, cursorESMID, 7)
	exec = cursorAppendStringRaw(exec, cursorESMExecID, "exec-shell")
	exec = cursorAppendBytes(exec, cursorESMShell, cursorAppendString(
		cursorAppendString(nil, cursorSHACommand, "rm -rf /"), cursorSHAWorkingDirectory, "/tmp"))
	builtin := cursorAppendBytes(nil, cursorASMExecServer, exec)

	received := make(chan string, 1)
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		<-frames
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, builtin)
		select {
		case frame := <-frames:
			received <- describeCursorFrame(frame)
		case <-time.After(5 * time.Second):
			received <- "timeout"
			return
		}
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "ok"))
		writeCursorFrame(writer, cursorTestTurnEnd())
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	select {
	case frame := <-received:
		if !strings.Contains(frame, cursorBuiltinRejectReason) || !strings.Contains(frame, "exec-shell") {
			t.Fatalf("builtin rejection = %s", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the builtin was never rejected")
	}
	// The tool never surfaces to Claude Code: only the model's own text does.
	if strings.Contains(body, "rm -rf") {
		t.Fatalf("builtin call leaked to Claude Code:\n%s", body)
	}
}

func TestCursorStreamFailureBeforeFirstBlockSurfacesAsStatus(t *testing.T) {
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		<-frames
		writer.WriteHeader(http.StatusTooManyRequests)
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "slow down")
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))
	defer service.closeParked()

	oldBackoff := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = nil
	t.Cleanup(func() { upstreamFastRetryBackoff = oldBackoff })

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s, want the upstream 429", status, body)
	}
	if strings.Contains(body, "event: message_start") {
		t.Fatalf("a failed stream must not open, body=%s", body)
	}
}

func TestCursorUnauthorizedRefreshesInsideTheAttempt(t *testing.T) {
	authorizer := cursorTestCredential(t, "stale-token")
	var refreshCalls int
	refreshServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refreshCalls++
		if request.Method != http.MethodPost {
			t.Errorf("refresh method = %s", request.Method)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh-token" {
			t.Errorf("refresh body = %v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"accessToken":"fresh-token","refreshToken":"refresh-token"}`)
	}))
	defer refreshServer.Close()
	oldTokenURL := cursorOAuthToken
	cursorOAuthToken = refreshServer.URL
	t.Cleanup(func() { cursorOAuthToken = oldTokenURL })

	var tokens []string
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		tokens = append(tokens, request.Header.Get("Authorization"))
		<-frames
		if len(tokens) == 1 {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "after refresh"))
		writeCursorFrame(writer, cursorTestTurnEnd())
	})
	service := newCursorTestService(t, authorizer)
	defer service.closeParked()

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if len(tokens) != 2 || tokens[0] != "Bearer stale-token" || tokens[1] != "Bearer fresh-token" {
		t.Fatalf("authorizations = %v", tokens)
	}
	if !strings.Contains(body, "after refresh") {
		t.Fatalf("stream missing the retried text:\n%s", body)
	}
	// The refreshed token is persisted for the next launch.
	raw, err := os.ReadFile(authorizer.path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["access_token"] != "fresh-token" {
		t.Fatalf("persisted access token = %v", metadata["access_token"])
	}
}

func TestCursorRetries429ThroughSharedFastRetry(t *testing.T) {
	var attempts int
	newCursorStubUpstream(t, func(writer http.ResponseWriter, request *http.Request, frames <-chan cursorStubFrame) {
		attempts++
		<-frames
		if attempts == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/connect+proto")
		writer.WriteHeader(http.StatusOK)
		writeCursorFrame(writer, cursorTestInteraction(cursorIUTextDelta, "second try"))
		writeCursorFrame(writer, cursorTestTurnEnd())
	})
	service := newCursorTestService(t, cursorTestCredential(t, "access-token"))

	oldBackoff := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = oldBackoff })

	status, body := serveCursorTurn(t, service, cursorTestStreamRequest(t, service))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !strings.Contains(body, "second try") {
		t.Fatalf("stream missing the retried text:\n%s", body)
	}
}

func TestCursorTakeParkedDropsUnrelatedStream(t *testing.T) {
	closed := make(chan struct{})
	_, writer := io.Pipe()
	session := &cursorSession{
		writer: writer,
		cancel: func() {},
		pending: map[string]cursorPendingCall{
			"toolu_1": {msgID: 1, execID: "exec-1"},
		},
		response: &http.Response{Body: closedReader{closed}},
	}
	service := &cursorService{parked: session, blobs: map[string][]byte{}}
	if got := service.takeParked([]string{"other"}); got != nil {
		t.Fatalf("takeParked returned an unrelated stream: %v", got)
	}
	service.mu.Lock()
	parked := service.parked
	service.mu.Unlock()
	if parked != nil {
		t.Fatal("an unrelated stream must not stay parked")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the dropped stream was not closed")
	}
}

// closedReader reports when its stream's body is closed.
type closedReader struct{ closed chan struct{} }

func (c closedReader) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c closedReader) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func TestCursorCloseDoesNotWaitForTheHeartbeat(t *testing.T) {
	writer := &blockingPipeWriter{writes: make(chan struct{}, 8)}
	session := newCursorSession(&http.Response{Body: io.NopCloser(strings.NewReader(""))}, writer, func() {})
	session.startHeartbeat()

	done := make(chan struct{})
	go func() {
		session.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close blocked on the heartbeat")
	}
	// The heartbeat exits on its own after close cancels it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		beat := session.beat
		session.mu.Unlock()
		if beat == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat was not detached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingPipeWriter accepts a frame then blocks, standing in for a stream whose
// send is in flight while the stream is closed.
type blockingPipeWriter struct {
	writes chan struct{}
}

func (w *blockingPipeWriter) Write(p []byte) (int, error) {
	select {
	case w.writes <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *blockingPipeWriter) CloseWithError(error) error { return nil }

func TestCursorBlobLookupAcceptsRawHashAndHex(t *testing.T) {
	service := &cursorService{}
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = byte(index)
	}
	service.storeBlob(raw, []byte("payload"))
	if got := service.blob(raw); string(got) != "payload" {
		t.Fatalf("raw lookup = %q", got)
	}
	if got := service.blob([]byte(hex.EncodeToString(raw))); string(got) != "payload" {
		t.Fatalf("hex lookup = %q", got)
	}
	if got := service.blob([]byte(strings.Repeat("ab", 32))); got != nil {
		t.Fatalf("unknown lookup = %q", got)
	}
}

func TestCursorModelAliasResolvesToTechnicalID(t *testing.T) {
	service := &cursorService{modelRoute: map[string]string{"composer-2-fast": "composer-2"}}
	converted := &cursorConvertedRequest{model: "composer-2"}
	converted.clientModel = "composer-2-fast"
	if got := service.upstreamModel(converted); got != "composer-2" {
		t.Fatalf("upstream model = %q, want composer-2", got)
	}
	converted.clientModel = "unknown"
	if got := service.upstreamModel(converted); got != "composer-2" {
		t.Fatalf("unknown alias = %q, want the requested model", got)
	}
}
