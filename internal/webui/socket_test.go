package webui

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shortTempDir keeps socket paths under the platform limit, which the
// default test directories on macOS exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ccl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenUnixReplacesOnlyStaleSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket file modes are POSIX")
	}
	dir := shortTempDir(t)
	path := filepath.Join(dir, "web", "ccl.sock")

	first, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", mode)
	}
	if _, err := ListenUnix(path); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second listener: err = %v, want already listening", err)
	}

	// A server that died leaves its socket behind; the next one replaces it.
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = first.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	second, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("replace stale socket: %v", err)
	}
	_ = second.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("closing the listener left %s: %v", path, err)
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUnix(plain); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: err = %v, want not a socket", err)
	}
	if _, err := ListenUnix(filepath.Join(dir, strings.Repeat("x", 120))); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("long path: err = %v, want longer than", err)
	}
}

// TestUnixSocketNeedsNoLoopbackHost: a native client may send any Host over
// the socket, and no browser page can reach it, so guard lets it through;
// the same request over TCP is still refused.
func TestUnixSocketNeedsNoLoopbackHost(t *testing.T) {
	server, err := New(Config{Workspace: t.TempDir(), StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shortTempDir(t), "ccl.sock")
	listener, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	request, _ := http.NewRequest(http.MethodPost, "http://ccl.example/api/sessions", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		t.Fatalf("socket request refused: %s", body)
	}

	tcp := newHTTPTestServer(t, server.Handler())
	request, _ = http.NewRequest(http.MethodGet, tcp+"/api/sessions", nil)
	request.Host = "ccl.example"
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("TCP request with a foreign Host = %d, want 403", response.StatusCode)
	}
}
