package webui

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeBasePath(t *testing.T) {
	for value, want := range map[string]string{"": "", "/": "", "/chat": "/chat", "/chat/": "/chat", "/a/b-c_d.e~": "/a/b-c_d.e~"} {
		if got, err := NormalizeBasePath(value); err != nil || got != want {
			t.Fatalf("NormalizeBasePath(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"chat", "//chat", "/chat//x", "/./chat", "/chat/..", "/chat%2Fx", "/ch at", "/chat?x=1", "/chat#x", `/chat\x`, "https://evil.example/chat", "/health", "/health/"} {
		if got, err := NormalizeBasePath(value); err == nil {
			t.Fatalf("NormalizeBasePath(%q) = %q, want an error", value, got)
		}
	}
	if _, err := New(Config{BasePath: "/../x", Workspace: t.TempDir(), StoreRoot: t.TempDir()}); err == nil {
		t.Fatal("New accepted an unsafe base path")
	}
}

// noRedirect returns the redirect itself, so its Location can be checked.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func fetch(t *testing.T, method, url string, header map[string]string, body string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range header {
		request.Header.Set(key, value)
	}
	response, err := noRedirect.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response, string(data)
}

func TestBasePathMountsPageAndAPI(t *testing.T) {
	base := testServer(t, Config{Token: "secret", BasePath: "/chat/"}).URL
	auth := map[string]string{PageTokenHeader: "secret"}

	// /chat itself moves to /chat/ for good, keeping the query.
	response, _ := fetch(t, "GET", base+"/chat?x=1", nil, "")
	if response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") != "/chat/?x=1" {
		t.Fatalf("/chat = %d %q", response.StatusCode, response.Header.Get("Location"))
	}

	// The page carries the prefix as its base; its validator is the rendered page's.
	response, page := fetch(t, "GET", base+"/chat/", nil, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(page, `<base href="/chat/" data-ccl-base>`) || strings.Contains(page, `<base href="/" data-ccl-base>`) {
		t.Fatalf("page = %d %s", response.StatusCode, page)
	}
	etag := response.Header.Get("ETag")
	if etag == "" || etag != contentETag([]byte(page)) || response.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("page ETag = %q, Cache-Control = %q", etag, response.Header.Get("Cache-Control"))
	}
	if response, _ := fetch(t, "GET", base+"/chat/", map[string]string{"If-None-Match": etag}, ""); response.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidated page = %d", response.StatusCode)
	}
	root := testServer(t, Config{Token: "secret"}).URL
	rootResponse, rootPage := fetch(t, "GET", root+"/", nil, "")
	if !strings.Contains(rootPage, `<base href="/" data-ccl-base>`) || rootResponse.Header.Get("ETag") == etag {
		t.Fatalf("root page base or ETag not distinct: %q", rootResponse.Header.Get("ETag"))
	}

	// A share link deep-links into the same page.
	response, page = fetch(t, "GET", base+"/chat/share/"+strings.Repeat("a", 64), nil, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(page, `<base href="/chat/"`) || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("share page = %d", response.StatusCode)
	}

	// Built assets live under the prefix and keep their long cache.
	asset := ""
	for _, field := range strings.Split(page, `"`) {
		if strings.HasPrefix(field, "./assets/") && strings.HasSuffix(field, ".js") {
			asset = strings.TrimPrefix(field, "./")
		}
	}
	if asset == "" {
		t.Fatal("the page references no relative script")
	}
	if response, _ := fetch(t, "GET", base+"/chat/"+asset, nil, ""); response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d %q", response.StatusCode, response.Header.Get("Cache-Control"))
	}

	// Health answers at both places; the API only under the prefix.
	for _, path := range []string{"/health", "/chat/health"} {
		if response, body := fetch(t, "GET", base+path, nil, ""); response.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("%s = %d %s", path, response.StatusCode, body)
		}
	}
	for _, path := range []string{"/api/sessions", "/", "/assets/" + asset, "/chatty/", "/chatty/api/sessions", "/share/" + strings.Repeat("a", 64)} {
		if response, _ := fetch(t, "GET", base+path, auth, ""); response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s outside the prefix = %d", path, response.StatusCode)
		}
	}
	if response, body := fetch(t, "GET", base+"/chat/api/sessions", auth, ""); response.StatusCode != http.StatusOK || !strings.HasPrefix(body, "[") {
		t.Fatalf("sessions = %d %s", response.StatusCode, body)
	}
	if response, _ := fetch(t, "GET", base+"/chat/api/sessions", nil, ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sessions without a token = %d", response.StatusCode)
	}

	// ServeMux's own redirects stay under the prefix.
	for _, path := range []string{"/chat/api/files", "/chat/api//sessions", "/chat/api/./sessions"} {
		response, _ := fetch(t, "GET", base+path, auth, "")
		if location := response.Header.Get("Location"); response.StatusCode/100 != 3 || !strings.HasPrefix(location, "/chat/") {
			t.Fatalf("%s = %d to %q", path, response.StatusCode, location)
		}
	}

	// The guard sees the root path: a write still has to say it is JSON.
	if response, _ := fetch(t, "POST", base+"/chat/api/sessions", map[string]string{PageTokenHeader: "secret", "Content-Type": "text/plain"}, `{}`); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain write = %d", response.StatusCode)
	}

	// A share link names the prefix it is served under.
	response, body := fetch(t, "POST", base+"/chat/api/sessions", map[string]string{PageTokenHeader: "secret", "Content-Type": "application/json"}, `{"title":"s"}`)
	var session SessionView
	if response.StatusCode != http.StatusCreated || json.Unmarshal([]byte(body), &session) != nil {
		t.Fatalf("create = %d %s", response.StatusCode, body)
	}
	response, body = fetch(t, "POST", base+"/chat/api/sessions/"+session.ID+"/share", map[string]string{PageTokenHeader: "secret", "Content-Type": "application/json"}, `{}`)
	var link struct{ Token, Path string }
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &link) != nil || link.Path != "/chat/share/"+link.Token {
		t.Fatalf("share = %d %s", response.StatusCode, body)
	}
	if response, _ := fetch(t, "GET", base+"/chat/api/share/"+link.Token, nil, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("shared conversation = %d", response.StatusCode)
	}
}

func TestPageTokenHeaderLeavesAuthorizationToAProxy(t *testing.T) {
	base := testServer(t, Config{Token: "secret"}).URL
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:password"))
	for _, tc := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"page token behind Basic auth", map[string]string{PageTokenHeader: "secret", "Authorization": basic}, http.StatusOK},
		{"page token alone", map[string]string{PageTokenHeader: "secret"}, http.StatusOK},
		{"Bearer, as before", map[string]string{"Authorization": "Bearer secret"}, http.StatusOK},
		{"Basic auth is no token", map[string]string{"Authorization": basic}, http.StatusUnauthorized},
		{"a wrong page token is not rescued by Bearer", map[string]string{PageTokenHeader: "wrong", "Authorization": "Bearer secret"}, http.StatusUnauthorized},
		{"an empty page token is not rescued by Bearer", map[string]string{PageTokenHeader: "", "Authorization": "Bearer secret"}, http.StatusUnauthorized},
		{"nothing", nil, http.StatusUnauthorized},
	} {
		if response, _ := fetch(t, "GET", base+"/api/sessions", tc.header, ""); response.StatusCode != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, response.StatusCode, tc.want)
		}
	}
	// The query token still opens only the read-only routes.
	if response, _ := fetch(t, "GET", base+"/api/sessions?token=secret", nil, ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token on a private route = %d", response.StatusCode)
	}
}

func TestBasePathStreamsFramesAtOnce(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret", BasePath: "/chat", Launch: helperPrepare(t,
		`{"type":"system","subtype":"init","session_id":"stream-prefix"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1,"output_tokens":2}}`, false)})
	base := httpServer.URL + "/chat"
	jsonHeader := map[string]string{PageTokenHeader: "secret", "Content-Type": "application/json"}
	response, body := fetch(t, "POST", base+"/api/sessions", jsonHeader, `{"title":"s"}`)
	var session SessionView
	if response.StatusCode != http.StatusCreated || jsonUnmarshal(body, &session) != nil {
		t.Fatalf("create = %d %s", response.StatusCode, body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/sessions/"+session.ID+"/stream?since=0&token=secret", nil)
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	if response, body := fetch(t, "POST", base+"/api/sessions/"+session.ID+"/messages", jsonHeader, `{"text":"hello"}`); response.StatusCode != http.StatusAccepted {
		t.Fatalf("message = %d %s", response.StatusCode, body)
	}
	reader := bufio.NewReader(stream.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the result: %v", err)
		}
		if strings.HasPrefix(line, "event: "+EventResult) {
			break
		}
	}
	waitIdle(t, base, session.ID)
}

func jsonUnmarshal(body string, value any) error { return json.Unmarshal([]byte(body), value) }
