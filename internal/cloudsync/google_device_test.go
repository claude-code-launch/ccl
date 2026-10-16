package cloudsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeGoogleDeviceServer answers the device and token endpoints. The first
// token poll is pending, as when the user has not approved yet.
func fakeGoogleDeviceServer(t *testing.T) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	var mu sync.Mutex
	var tokenRequests []map[string]string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device/code":
			if r.Form.Get("client_id") != "device-client" || r.Form.Get("scope") != googleDriveScope {
				t.Errorf("device request: %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device-secret-code", "user_code": "WDJB-MJHT",
				"verification_url": "https://www.google.com/device", "expires_in": 600, "interval": 1,
			})
		case "/token":
			client, secret, ok := r.BasicAuth()
			if !ok {
				client, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
			}
			mu.Lock()
			tokenRequests = append(tokenRequests, map[string]string{
				"grant_type": r.Form.Get("grant_type"), "client_id": client, "client_secret": secret,
			})
			polls++
			pending := r.Form.Get("grant_type") != "refresh_token" && polls == 1
			mu.Unlock()
			if pending {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-" + r.Form.Get("grant_type"), "refresh_token": "device-refresh",
				"token_type": "Bearer", "expires_in": 3600,
			})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	original := googleOAuthEndpoint
	googleOAuthEndpoint = oauth2.Endpoint{
		AuthURL: server.URL + "/auth", TokenURL: server.URL + "/token", DeviceAuthURL: server.URL + "/device/code",
	}
	t.Cleanup(func() { googleOAuthEndpoint = original })
	t.Setenv("CCL_GOOGLE_DEVICE_CLIENT_ID", "device-client")
	t.Setenv("CCL_GOOGLE_DEVICE_CLIENT_SECRET", "device-secret")
	return server, &tokenRequests
}

func TestGoogleDeviceFlowShowsAQRCodeAndKeepsItsClient(t *testing.T) {
	_, requests := fakeGoogleDeviceServer(t)
	originalOpen := openBrowserURL
	openBrowserURL = func(string) error { t.Fatal("device flow opened a browser"); return nil }
	t.Cleanup(func() { openBrowserURL = originalOpen })
	authPath := filepath.Join(t.TempDir(), googleAuthName)
	var notice strings.Builder
	ctx := WithDeviceAuthorization(context.Background(), true)
	if _, err := authorizeGoogleDriveAt(ctx, authPath, &notice); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"WDJB-MJHT", "https://www.google.com/device", "▀", "Waiting for approval"} {
		if !strings.Contains(notice.String(), text) {
			t.Fatalf("notice missing %q:\n%s", text, notice.String())
		}
	}
	stored, err := loadGoogleAuth(authPath)
	if err != nil || stored.Client != googleClientDevice || stored.Token.RefreshToken != "device-refresh" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	// Refreshing a device-flow token must use the device client, and keep
	// recording it, or Google rejects the refresh as a client mismatch.
	stored.Token.Expiry = time.Now().Add(-time.Hour)
	if err := saveGoogleAuth(authPath, stored.Token, stored.Client); err != nil {
		t.Fatal(err)
	}
	remote, err := loadAuthorizedGoogleDriveAt(context.Background(), authPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.client.Transport.(*oauth2.Transport).Source.Token(); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last["grant_type"] != "refresh_token" || last["client_id"] != "device-client" || last["client_secret"] != "device-secret" {
		t.Fatalf("refresh request = %v", last)
	}
	if again, _ := loadGoogleAuth(authPath); again.Client != googleClientDevice {
		t.Fatalf("refresh dropped the client: %+v", again)
	}
}

func TestGoogleDeviceFlowNeedsTheDeviceClient(t *testing.T) {
	t.Setenv("CCL_GOOGLE_DEVICE_CLIENT_ID", "")
	original := googleDeviceClientID
	googleDeviceClientID = ""
	t.Cleanup(func() { googleDeviceClientID = original })
	if DeviceAuthorizationAvailable() {
		t.Fatal("device flow reported available without a client")
	}
	ctx := WithDeviceAuthorization(context.Background(), true)
	_, err := authorizeGoogleDriveAt(ctx, filepath.Join(t.TempDir(), googleAuthName), nil)
	if err == nil || !strings.Contains(err.Error(), "--device=false") {
		t.Fatalf("error = %v", err)
	}
}
