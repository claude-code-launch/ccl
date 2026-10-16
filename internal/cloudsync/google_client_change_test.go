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
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// fakeGoogleProjects serves OAuth and Drive for several Cloud projects. Each
// OAuth client gets its own appDataFolder, as on Google, because the access
// token it issues names the client.
type fakeGoogleProjects struct {
	mu      sync.Mutex
	bundles map[string][]byte
	version int
}

func newFakeGoogleProjects(t *testing.T) *fakeGoogleProjects {
	t.Helper()
	fake := &fakeGoogleProjects{bundles: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	originalEndpoint, originalOpen := googleOAuthEndpoint, openBrowserURL
	originalAPI, originalUpload := googleDriveAPIBase, googleDriveUploadBase
	googleOAuthEndpoint = oauth2.Endpoint{AuthURL: server.URL + "/auth", TokenURL: server.URL + "/token"}
	googleDriveAPIBase, googleDriveUploadBase = server.URL, server.URL
	openBrowserURL = func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		go func() {
			_, _ = http.Get(query.Get("redirect_uri") + "?state=" + url.QueryEscape(query.Get("state")) +
				"&code=" + url.QueryEscape(query.Get("client_id")))
		}()
		return nil
	}
	t.Cleanup(func() {
		googleOAuthEndpoint, openBrowserURL = originalEndpoint, originalOpen
		googleDriveAPIBase, googleDriveUploadBase = originalAPI, originalUpload
	})
	return fake
}

func (fake *fakeGoogleProjects) serve(w http.ResponseWriter, r *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		client, _, _ := r.BasicAuth()
		if client == "" {
			client = r.Form.Get("client_id")
		}
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") != client {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access:" + client, "refresh_token": "refresh:" + client,
			"token_type": "Bearer", "expires_in": 3600,
		})
		return
	}
	project := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access:")
	file := func() map[string]string {
		return map[string]string{"id": "bundle:" + project, "name": googleBundleName, "version": strconv.Itoa(fake.version)}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files":
		files := []map[string]string{}
		if fake.bundles[project] != nil {
			files = append(files, file())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files/bundle:"+project:
		_, _ = w.Write(fake.bundles[project])
	case r.Method == http.MethodPost && r.URL.Path == "/upload/drive/v3/files":
		_, params, _ := strings.Cut(r.Header.Get("Content-Type"), "boundary=")
		reader := multipart.NewReader(r.Body, params)
		_, _ = reader.NextPart()
		media, _ := reader.NextPart()
		fake.bundles[project], _ = io.ReadAll(media)
		fake.version++
		_ = json.NewEncoder(w).Encode(file())
	case r.Method == http.MethodPatch && r.URL.Path == "/upload/drive/v3/files/bundle:"+project:
		fake.bundles[project], _ = io.ReadAll(r.Body)
		fake.version++
		_ = json.NewEncoder(w).Encode(file())
	default:
		http.NotFound(w, r)
	}
}

func (fake *fakeGoogleProjects) profile(t *testing.T, project string) remoteProfile {
	t.Helper()
	fake.mu.Lock()
	data := fake.bundles[project]
	fake.mu.Unlock()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("%s bundle: %v", project, err)
	}
	for _, entry := range archive.File {
		if entry.Name == profileFileName {
			reader, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var profile remoteProfile
			if err := json.NewDecoder(reader).Decode(&profile); err != nil {
				t.Fatal(err)
			}
			return profile
		}
	}
	t.Fatalf("%s bundle has no profile", project)
	return remoteProfile{}
}

// TestGoogleLoginOpensTheAccountsProfileOnANewDevice covers the whole Google
// flow: the first device creates the profile in the account's appDataFolder,
// and a second device opens it with nothing but its own login.
func TestGoogleLoginOpensTheAccountsProfileOnANewDevice(t *testing.T) {
	fake := newFakeGoogleProjects(t)
	ctx := context.Background()
	project := googleOAuthClientID

	t.Setenv("HOME", t.TempDir())
	first, err := LoginGoogleDriveNamed(ctx, "google-drive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing || first.RemoteDir != googleDriveLabel {
		t.Fatalf("first login = %+v", first)
	}
	profile := fake.profile(t, project)
	if profile.AccountKey == "" {
		t.Fatalf("uploaded profile has no key: %+v", profile)
	}
	if _, err := LoginGoogleDriveNamed(ctx, "google-drive", nil); err != nil {
		t.Fatalf("repeated login: %v", err)
	}

	t.Setenv("HOME", t.TempDir())
	joined, err := LoginGoogleDriveNamed(ctx, "google-drive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !joined.Existing || joined.RemoteDir != googleDriveLabel {
		t.Fatalf("new device login = %+v", joined)
	}
	if manager, err := Load(); err != nil || manager.profileID != profile.ID {
		t.Fatalf("new device profile = %v, %v", manager, err)
	}
}
