package cloudsync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

func TestGooglePastedRedirectCompletesLoginWithoutABrowser(t *testing.T) {
	var exchanged url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		exchanged = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()
	originalEndpoint, originalOpen := googleOAuthEndpoint, openBrowserURL
	googleOAuthEndpoint = oauth2.Endpoint{AuthURL: server.URL + "/auth", TokenURL: server.URL + "/token"}
	openBrowserURL = func(string) error { t.Error("pasted-redirect login opened a browser"); return nil }
	t.Cleanup(func() { googleOAuthEndpoint, openBrowserURL = originalEndpoint, originalOpen })

	in, paste := io.Pipe()
	defer paste.Close()
	notice := &lockedBuffer{}
	go func() {
		pattern := regexp.MustCompile(`state=([^&\s]+)`)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if match := pattern.FindStringSubmatch(notice.String()); match != nil {
				// A stray line is rejected with a hint, then the address
				// copied from the browser (on another machine) completes it.
				_, _ = io.WriteString(paste, "4/not-a-url\n\n")
				_, _ = io.WriteString(paste, "http://127.0.0.1:1/oauth2/callback?state="+match[1]+
					"&code=pasted-code&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fdrive.appdata\n")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	ctx := WithPastedRedirect(context.Background(), in)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if _, err := authorizeGoogleDriveAt(ctx, authPath, notice); err != nil {
		t.Fatal(err)
	}
	if exchanged.Get("code") != "pasted-code" || exchanged.Get("code_verifier") == "" {
		t.Fatalf("token exchange = %v", exchanged)
	}
	for _, text := range []string{"Open this URL", "paste it here", "not the redirected address"} {
		if !strings.Contains(notice.String(), text) {
			t.Fatalf("notice missing %q:\n%s", text, notice.String())
		}
	}
	if token, err := loadGoogleToken(authPath); err != nil || token.RefreshToken != "refresh" {
		t.Fatalf("stored token = %+v, %v", token, err)
	}
}

func TestGooglePastedRedirectRejectsAnotherLoginsState(t *testing.T) {
	results := make(chan callbackResult, 1)
	readPastedGoogleRedirect(strings.NewReader("http://127.0.0.1:9/oauth2/callback?state=other&code=x\n"), "mine", nil, results)
	if result := <-results; result.err == nil || !strings.Contains(result.err.Error(), "state did not match") {
		t.Fatalf("result = %+v", result)
	}
}
