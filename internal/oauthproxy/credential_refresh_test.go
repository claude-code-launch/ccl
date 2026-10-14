package oauthproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexIndependentSessionsShareOneRotatingRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-test.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"fake-old","refresh_token":"fake-refresh-old","expired":"2000-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if refreshes.Add(1) > 1 || r.Form.Get("refresh_token") != "fake-refresh-old" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		time.Sleep(60 * time.Millisecond) // leave the first refresh in flight
		_, _ = io.WriteString(w, `{"access_token":"fake-new","refresh_token":"fake-refresh-new","expires_in":3600}`)
	}))
	defer server.Close()
	previous := codexOAuthTokenURL
	codexOAuthTokenURL = server.URL
	defer func() { codexOAuthTokenURL = previous }()
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		a := &codexOAuthAuthorizer{path: path, client: server.Client()}
		go func() {
			<-start
			auth, err := a.authorize(context.Background(), false)
			if err == nil && auth.token != "fake-new" {
				err = fmt.Errorf("did not reuse refreshed access token")
			}
			results <- err
		}()
	}
	close(start)
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	// A delayed 401 from an old in-flight request must not rotate the new token.
	a := &codexOAuthAuthorizer{path: path, client: server.Client()}
	if _, err := a.authorize(withRejectedOAuthToken(context.Background(), "fake-old"), true); err != nil {
		t.Fatal(err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh calls=%d, want one", got)
	}
}

func TestCredentialLockSurvivesAuthDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	auth := filepath.Join(root, "auth")
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(auth, "fake.json")
	release, err := lockOAuthCredential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.Rename(auth, filepath.Join(root, "old-auth")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := lockOAuthCredential(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("directory replacement lost lock: %v", err)
	}
}
