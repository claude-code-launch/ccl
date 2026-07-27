package cloudsync

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestGoogleOAuthUsesBuiltInClientAndBrowserPKCE(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalEndpoint := googleOAuthEndpoint
	originalOpen := openBrowserURL
	t.Cleanup(func() {
		googleOAuthEndpoint = originalEndpoint
		openBrowserURL = originalOpen
	})

	var tokenRequest url.Values
	var tokenClientID, tokenClientSecret string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			tokenRequest = request.Form
			tokenClientID, tokenClientSecret, _ = request.BasicAuth()
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	googleOAuthEndpoint = oauth2.Endpoint{
		AuthURL:  server.URL + "/auth",
		TokenURL: server.URL + "/token",
	}
	openBrowserURL = func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		if got := parsed.Query().Get("client_id"); got != googleOAuthClientID {
			t.Errorf("client_id = %q", got)
		}
		if parsed.Query().Get("code_challenge") == "" {
			t.Error("authorization URL has no PKCE challenge")
		}
		callback := parsed.Query().Get("redirect_uri")
		state := parsed.Query().Get("state")
		go func() {
			_, _ = http.Get(callback + "?state=" + url.QueryEscape(state) + "&code=authorized")
		}()
		return nil
	}

	if _, err := authorizeGoogleDrive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokenClientID == "" {
		tokenClientID = tokenRequest.Get("client_id")
	}
	if tokenClientID != googleOAuthClientID {
		t.Fatalf("token client_id = %q", tokenClientID)
	}
	// Google's installed Desktop client still requires client_secret on the
	// token endpoint even though the value is public; PKCE is the real
	// authorization-code protection.
	gotSecret := tokenClientSecret
	if gotSecret == "" {
		gotSecret = tokenRequest.Get("client_secret")
	}
	if gotSecret == "" {
		t.Fatal("desktop OAuth flow omitted client_secret required by Google token endpoint")
	}
	if want := strings.TrimSpace(googleOAuthClientSecret); want != "" && gotSecret != want {
		t.Fatalf("token client_secret = %q, want built-in desktop secret", gotSecret)
	}
	if tokenRequest.Get("code_verifier") == "" {
		t.Fatal("token exchange has no PKCE verifier")
	}
	authPath := filepath.Join(os.Getenv("HOME"), ".ccl", googleAuthName)
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Google auth mode = %o", info.Mode().Perm())
	}
}

func TestGoogleDriveBundleCreateUpdateAndDownload(t *testing.T) {
	var (
		mu       sync.Mutex
		fileID   string
		uploaded []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/drive/v3/files":
			writer.Header().Set("Content-Type", "application/json")
			if fileID == "" {
				_, _ = io.WriteString(writer, `{"files":[]}`)
			} else {
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"files": []map[string]string{{"id": fileID, "name": googleBundleName}},
				})
			}
		case request.Method == http.MethodPost && request.URL.Path == "/upload/drive/v3/files":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
			}
			contentType := request.Header.Get("Content-Type")
			boundary := strings.TrimPrefix(contentType, "multipart/related; boundary=")
			if boundary == contentType {
				t.Errorf("unexpected upload content type %q", contentType)
			}
			parts := multipartParts(t, body, boundary)
			if len(parts) != 2 {
				t.Fatalf("multipart part count = %d", len(parts))
			}
			uploaded = parts[1]
			fileID = "bundle-id"
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"bundle-id"}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/upload/drive/v3/files/bundle-id":
			uploaded, _ = io.ReadAll(request.Body)
			_, _ = io.WriteString(writer, `{}`)
		case request.Method == http.MethodGet && request.URL.Path == "/drive/v3/files/bundle-id":
			_, _ = writer.Write(uploaded)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	originalAPI, originalUpload := googleDriveAPIBase, googleDriveUploadBase
	googleDriveAPIBase, googleDriveUploadBase = server.URL, server.URL
	t.Cleanup(func() {
		googleDriveAPIBase, googleDriveUploadBase = originalAPI, originalUpload
	})

	remote := &googleDriveRemote{client: server.Client()}
	cache := filepath.Join(t.TempDir(), googleCacheName)
	if err := os.MkdirAll(filepath.Join(cache, snapshotsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"version":1,"id":"profile"}`)
	if err := os.WriteFile(filepath.Join(cache, profileFileName), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, verifierFileName), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := remote.uploadBundle(cache); err != nil {
		t.Fatal(err)
	}
	if fileID != "bundle-id" || len(uploaded) == 0 {
		t.Fatal("Google Drive create did not capture the bundle")
	}
	if err := remote.uploadBundle(cache); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), googleCacheName)
	found, err := remote.downloadBundle(target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("download did not find bundle")
	}
	got, err := os.ReadFile(filepath.Join(target, profileFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, profile) {
		t.Fatalf("downloaded profile = %q", got)
	}
}

func TestGoogleBundleRejectsTraversal(t *testing.T) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	part, err := writer.Create("../cloud.key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), googleCacheName)
	if err := replaceGoogleCache(target, output.Bytes()); err == nil ||
		!strings.Contains(err.Error(), "unsupported path") {
		t.Fatalf("traversal bundle error = %v", err)
	}
}

func multipartParts(t *testing.T, body []byte, boundary string) [][]byte {
	t.Helper()
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var result [][]byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, data)
	}
}

func TestSavingTokenSourceKeepsRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), googleAuthName)
	previous := &oauth2.Token{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}
	source := &savingTokenSource{
		source: oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: "new",
			Expiry:      time.Now().Add(time.Hour),
		}),
		authPath: path,
		last:     previous,
	}
	token, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "refresh" {
		t.Fatalf("refresh token = %q", token.RefreshToken)
	}
	stored, err := loadGoogleToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh" {
		t.Fatalf("stored refresh token = %q", stored.RefreshToken)
	}
}

func TestGoogleOAuthClientSecretCanBeInjectedAtBuildOrDevelopmentTime(t *testing.T) {
	original := googleOAuthClientSecret
	t.Cleanup(func() {
		googleOAuthClientSecret = original
		_ = os.Unsetenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET")
	})

	// Built-in Desktop secret is always present so local builds work without
	// ldflags or env. Empty env must fall back to the package default.
	googleOAuthClientSecret = original
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET", "")
	if got := googleOAuthConfig("http://127.0.0.1/callback").ClientSecret; got == "" {
		t.Fatal("expected built-in Desktop OAuth client secret")
	} else if got != original {
		t.Fatalf("default OAuth client secret = %q, want built-in value", got)
	}

	// Development env overrides the built-in/default package value.
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET", "development-secret")
	if got := googleOAuthConfig("http://127.0.0.1/callback").ClientSecret; got != "development-secret" {
		t.Fatalf("OAuth client secret = %q", got)
	}

	// Release -ldflags override only applies when env is unset.
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET", "")
	googleOAuthClientSecret = "release-secret"
	if got := googleOAuthConfig("http://127.0.0.1/callback").ClientSecret; got != "release-secret" {
		t.Fatalf("release OAuth client secret = %q", got)
	}
}

func TestGoogleDriveResponseErrorIsCompact(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(recorder, `{
		"error": {
			"code": 403,
			"message": "Drive API is disabled.",
			"status": "PERMISSION_DENIED",
			"details": [{"large":"diagnostic payload"}]
		}
	}`)
	response := recorder.Result()
	defer response.Body.Close()
	err := googleDriveResponseError("list application data", response)
	if err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED: Drive API is disabled.") {
		t.Fatalf("compact error = %v", err)
	}
	if strings.Contains(err.Error(), "diagnostic payload") {
		t.Fatalf("error included verbose API details: %v", err)
	}
}
