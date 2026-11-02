package oauthproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCursorRefreshRefusesCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", sink.URL+"/?token=fake-hidden")
		w.WriteHeader(307)
	}))
	defer source.Close()
	previous := cursorOAuthToken
	cursorOAuthToken = source.URL
	t.Cleanup(func() { cursorOAuthToken = previous })
	authorizer := &cursorOAuthAuthorizer{path: filepath.Join(t.TempDir(), "cursor.json"), client: &http.Client{Timeout: time.Second, CheckRedirect: checkCredentialRedirect}}
	_, err := authorizer.refresh(context.Background(), &cursorOAuthCredential{accessToken: "fake-old", refreshToken: "fake-refresh", metadata: map[string]any{}})
	if err == nil || leaked.Load() != 0 || strings.Contains(err.Error(), "fake-hidden") {
		t.Fatalf("unsafe redirect: %v %d", err, leaked.Load())
	}
}

func TestCursorPollNetworkErrorDoesNotExposeVerifier(t *testing.T) {
	source := httptest.NewServer(http.NotFoundHandler())
	source.Close()
	previous := cursorPollURL
	cursorPollURL = source.URL
	t.Cleanup(func() { cursorPollURL = previous })
	_, _, err := pollCursorOnce(context.Background(), &http.Client{Timeout: time.Second}, cursorLoginParams{UUID: "fake-state", Verifier: "fake-secret-verifier"})
	if err == nil || strings.Contains(err.Error(), "fake-secret-verifier") {
		t.Fatalf("poll error exposed verifier: %v", err)
	}
}
