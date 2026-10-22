package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func guardedRequest(t *testing.T, server *Server, method, target, host, origin, contentType, body string) int {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder.Code
}

func TestGuardRefusesRebindingAndCrossSiteWithoutToken(t *testing.T) {
	server, err := New(Config{Workspace: t.TempDir(), StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, method, host, origin, contentType string
		want                                    int
	}{
		{"loopback page", "GET", "127.0.0.1:8099", "", "", http.StatusOK},
		{"localhost page", "GET", "localhost:8099", "", "", http.StatusOK},
		{"ipv6 loopback", "GET", "[::1]:8099", "", "", http.StatusOK},
		// A rebinding page reaches 127.0.0.1 under its own name.
		{"rebound host", "GET", "evil.example:8099", "", "", http.StatusForbidden},
		{"rebound write", "POST", "evil.example", "http://evil.example", "application/json", http.StatusForbidden},
		// A page elsewhere posting to the real loopback address.
		{"cross-site write", "POST", "127.0.0.1:8099", "https://evil.example", "application/json", http.StatusForbidden},
		{"dev server write", "POST", "127.0.0.1:8099", "http://localhost:5173", "application/json", http.StatusCreated},
		{"cli write", "POST", "127.0.0.1:8099", "", "application/json", http.StatusCreated},
		// A form-style body never reaches a JSON handler.
		{"text body", "POST", "127.0.0.1:8099", "", "text/plain", http.StatusUnsupportedMediaType},
	} {
		target := "/api/sessions"
		if c.method == "GET" {
			target = "/health"
		}
		if got := guardedRequest(t, server, c.method, target, c.host, c.origin, c.contentType, `{}`); got != c.want {
			t.Errorf("%s: %s %s Host=%s Origin=%s = %d, want %d", c.name, c.method, target, c.host, c.origin, got, c.want)
		}
	}
}

func TestGuardLeavesTokenModeToTheToken(t *testing.T) {
	server, err := New(Config{Token: "secret", RequireToken: true, Workspace: t.TempDir(), StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A LAN or container address is fine once a token is required.
	if got := guardedRequest(t, server, "GET", "/health", "192.168.1.20:8099", "", "", ""); got != http.StatusOK {
		t.Fatalf("LAN host with a token = %d", got)
	}
	request := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(`{}`))
	request.Host = "192.168.1.20:8099"
	request.Header.Set("Origin", "http://192.168.1.20:8099")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("authorized LAN write = %d: %s", recorder.Code, recorder.Body)
	}
}
