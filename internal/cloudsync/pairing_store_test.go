package cloudsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// pairingStoreFixture is a valid request the file store can accept, kept small
// so a test can tweak one field and re-check the refusal.
func pairingStoreFixture(t *testing.T) pairingRequestEnvelope {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pairingPublicKey(masterKey, profileID)
	if err != nil {
		t.Fatal(err)
	}
	profile := remoteProfile{
		Version: formatVersion, ID: profileID,
		KDF: kdfMasterKey, PairingPublicKey: public,
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := newPairingRequest(profile, deviceID, "Store test device")
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// TestFilePairingStoreRoundTrip drives the store the way the pairing commands
// do: publish a request, list it back, answer it, and delete both halves.
func TestFilePairingStoreRoundTrip(t *testing.T) {
	remoteDir := t.TempDir()
	store, err := newFilePairingStore(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newFilePairingStore("relative/dir"); err == nil ||
		!strings.Contains(err.Error(), "invalid pairing store path") {
		t.Fatalf("relative store path error = %v", err)
	}

	ctx := t.Context()
	request := pairingStoreFixture(t)
	if err := store.PutRequest(ctx, request); err != nil {
		t.Fatal(err)
	}

	// A duplicate publish is refused rather than silently overwriting the
	// pending request the other device is waiting on.
	if err := store.PutRequest(ctx, request); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate PutRequest() error = %v", err)
	}

	requests, err := store.ListRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].RequestID != request.RequestID ||
		requests[0].ProfileID != request.ProfileID {
		t.Fatalf("ListRequests() = %+v", requests)
	}

	// No response exists yet.
	if _, found, err := store.GetResponse(ctx, request.RequestID); err != nil || found {
		t.Fatalf("GetResponse() before approval = found %v, %v", found, err)
	}

	response := pairingResponseEnvelope{
		Version: pairingProtocolVersion, RequestID: request.RequestID,
		ProfileID: request.ProfileID, Nonce: "bm9uY2U", Ciphertext: "Y2lwaGVy",
		CreatedAt: request.CreatedAt, ExpiresAt: request.ExpiresAt,
	}
	if err := store.PutResponse(ctx, response); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.GetResponse(ctx, request.RequestID)
	if err != nil || !found {
		t.Fatalf("GetResponse() = found %v, %v", found, err)
	}
	if stored.RequestID != request.RequestID || stored.Ciphertext != response.Ciphertext {
		t.Fatalf("stored response = %+v", stored)
	}

	if err := store.Delete(ctx, request.RequestID); err != nil {
		t.Fatal(err)
	}
	if requests, err := store.ListRequests(ctx); err != nil || len(requests) != 0 {
		t.Fatalf("after Delete: %+v, %v", requests, err)
	}
	if _, found, err := store.GetResponse(ctx, request.RequestID); err != nil || found {
		t.Fatalf("response survived Delete: found %v, %v", found, err)
	}
	// Deleting an already-deleted object is not an error, so a retried
	// approval cannot strand a pairing.
	if err := store.Delete(ctx, request.RequestID); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}

	// A missing directory lists as empty rather than failing.
	if requests, err := store.ListRequests(ctx); err != nil || len(requests) != 0 {
		t.Fatalf("empty store = %+v, %v", requests, err)
	}
}

// TestFilePairingStoreRejectsUnusableObjects covers the store's own guards:
// bad identifiers, malformed JSON, files whose name disagrees with their id,
// symlinks, and oversized objects.
func TestFilePairingStoreRejectsUnusableObjects(t *testing.T) {
	remoteDir := t.TempDir()
	store, err := newFilePairingStore(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	request := pairingStoreFixture(t)
	validID := request.RequestID

	for name, call := range map[string]func() error{
		"put response with a short id": func() error {
			return store.PutResponse(ctx, pairingResponseEnvelope{RequestID: "short"})
		},
		"get response with an invalid id": func() error {
			_, _, err := store.GetResponse(ctx, "not-hex-because-of-g")
			return err
		},
		"delete with a path escape": func() error {
			return store.Delete(ctx, "../../etc/passwd")
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "invalid pairing") {
			t.Fatalf("%s error = %v", name, err)
		}
	}

	// A request envelope that fails validation never reaches the disk.
	broken := request
	broken.RequestID = "short"
	if err := store.PutRequest(ctx, broken); err == nil {
		t.Fatal("an invalid request was stored")
	}

	requestsDir := filepath.Join(remoteDir, pairingDirName, "requests")
	if err := os.MkdirAll(requestsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An entry whose name is not an identifier is reported, not skipped.
	if err := os.WriteFile(filepath.Join(requestsDir, "not-an-id.ccl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests(ctx); err == nil ||
		!strings.Contains(err.Error(), "invalid request name") {
		t.Fatalf("invalid request name error = %v", err)
	}
	if err := os.Remove(filepath.Join(requestsDir, "not-an-id.ccl")); err != nil {
		t.Fatal(err)
	}

	// A file whose id does not match its name would let one request masquerade
	// as another.
	mismatched := request
	mismatched.RequestID = strings.Repeat("a", 32)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(requestsDir, mismatched.RequestID+".ccl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests(ctx); err == nil ||
		!strings.Contains(err.Error(), "does not match its id") {
		t.Fatalf("mismatched id error = %v", err)
	}
	if err := os.Remove(filepath.Join(requestsDir, mismatched.RequestID+".ccl")); err != nil {
		t.Fatal(err)
	}

	// Unreadable JSON and a symlink both stop the listing.
	if err := os.WriteFile(filepath.Join(requestsDir, validID+".ccl"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests(ctx); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Fatalf("broken JSON error = %v", err)
	}
	if err := os.Remove(filepath.Join(requestsDir, validID+".ccl")); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(remoteDir, "elsewhere.ccl")
	if err := os.WriteFile(elsewhere, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(requestsDir, validID+".ccl")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests(ctx); err == nil ||
		!strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlinked request error = %v", err)
	}
	if err := os.Remove(filepath.Join(requestsDir, validID+".ccl")); err != nil {
		t.Fatal(err)
	}

	// An oversized object is refused on both the write and the read side.
	oversized := make([]byte, maxPairingObjectSize+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(requestsDir, validID+".ccl"), oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests(ctx); err == nil ||
		!strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("oversized request error = %v", err)
	}
}

// TestFilePairingStoreRefusesTooManyRequests pins the fan-out guard: a remote
// directory filled with requests cannot make the listing unbounded.
func TestFilePairingStoreRefusesTooManyRequests(t *testing.T) {
	remoteDir := t.TempDir()
	store, err := newFilePairingStore(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	requestsDir := filepath.Join(remoteDir, pairingDirName, "requests")
	if err := os.MkdirAll(requestsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 257 {
		name := fmt.Sprintf("%032x.ccl", i)
		if err := os.WriteFile(filepath.Join(requestsDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ListRequests(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "too many requests") {
		t.Fatalf("oversized listing error = %v", err)
	}
}

// TestReadLimitedJSONFileGuards inspects the shared reader directly: a missing
// file reports os.IsNotExist so callers can branch on it, a directory is
// refused, and a bad payload surfaces as a decode error.
func TestReadLimitedJSONFileGuards(t *testing.T) {
	directory := t.TempDir()
	var target pairingRequestEnvelope
	if err := readLimitedJSONFile(filepath.Join(directory, "missing.ccl"), &target); !os.IsNotExist(err) {
		t.Fatalf("missing file error = %v", err)
	}
	if err := readLimitedJSONFile(directory, &target); err == nil ||
		!strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("directory error = %v", err)
	}
	path := filepath.Join(directory, "bad.ccl")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readLimitedJSONFile(path, &target); err == nil {
		t.Fatal("a non-JSON payload was accepted")
	}
}

// TestGooglePairingStoreRoundTrip drives the Drive-backed store against a stub
// API: publish, list, answer, fetch, and delete, plus the guards that stop a
// malformed object from being uploaded.
func TestGooglePairingStoreRoundTrip(t *testing.T) {
	var (
		objects = map[string][]byte{} // name -> media
		nextID  = 0
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/drive/v3/files":
			writer.Header().Set("Content-Type", "application/json")
			files := make([]map[string]string, 0, len(objects))
			for name := range objects {
				files = append(files, map[string]string{
					"id": "id-" + name, "name": name, "size": "1", "version": "1",
				})
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"files": files})
		case request.Method == http.MethodPost && request.URL.Path == "/upload/drive/v3/files":
			body, _ := io.ReadAll(request.Body)
			parts := multipartParts(t, body,
				strings.TrimPrefix(request.Header.Get("Content-Type"), "multipart/related; boundary="))
			if len(parts) != 2 {
				t.Errorf("upload parts = %d", len(parts))
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var metadata struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			if err := json.Unmarshal(parts[0], &metadata); err != nil {
				t.Errorf("upload metadata = %v", err)
			}
			if len(metadata.Parents) != 1 || metadata.Parents[0] != "appDataFolder" {
				t.Errorf("upload parents = %v", metadata.Parents)
			}
			objects[metadata.Name] = parts[1]
			nextID++
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"id":"created-%d"}`, nextID)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/drive/v3/files/id-"):
			name := strings.TrimPrefix(request.URL.Path, "/drive/v3/files/id-")
			media, ok := objects[name]
			if !ok {
				http.NotFound(writer, request)
				return
			}
			_, _ = writer.Write(media)
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/drive/v3/files/id-"):
			delete(objects, strings.TrimPrefix(request.URL.Path, "/drive/v3/files/id-"))
			_, _ = io.WriteString(writer, `{}`)
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

	store := &googlePairingStore{remote: &googleDriveRemote{client: server.Client()}}
	ctx := t.Context()
	request := pairingStoreFixture(t)
	if err := store.PutRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	// Publishing the same request twice would race the other device's poll.
	if err := store.PutRequest(ctx, request); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate PutRequest() error = %v", err)
	}

	requests, err := store.ListRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].RequestID != request.RequestID {
		t.Fatalf("ListRequests() = %+v, %v", requests, err)
	}
	if _, found, err := store.GetResponse(ctx, request.RequestID); err != nil || found {
		t.Fatalf("GetResponse() before approval = found %v, %v", found, err)
	}

	response := pairingResponseEnvelope{
		Version: pairingProtocolVersion, RequestID: request.RequestID,
		ProfileID: request.ProfileID, Nonce: "bm9uY2U", Ciphertext: "Y2lwaGVy",
		CreatedAt: request.CreatedAt, ExpiresAt: request.ExpiresAt,
	}
	if err := store.PutResponse(ctx, response); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.GetResponse(ctx, request.RequestID)
	if err != nil || !found || stored.Ciphertext != response.Ciphertext {
		t.Fatalf("GetResponse() = %+v, found %v, %v", stored, found, err)
	}
	if err := store.Delete(ctx, request.RequestID); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("objects after Delete = %v", objects)
	}
	if err := store.Delete(ctx, request.RequestID); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}

	// The validators still apply on the Drive path.
	if err := store.PutResponse(ctx, pairingResponseEnvelope{RequestID: "short"}); err == nil ||
		!strings.Contains(err.Error(), "invalid pairing response id") {
		t.Fatalf("short response id error = %v", err)
	}
	if _, _, err := store.GetResponse(ctx, "not-a-valid-id"); err == nil ||
		!strings.Contains(err.Error(), "invalid pairing request id") {
		t.Fatalf("invalid response id error = %v", err)
	}
	if err := store.Delete(ctx, "bad"); err == nil ||
		!strings.Contains(err.Error(), "invalid pairing request id") {
		t.Fatalf("invalid delete id error = %v", err)
	}
	broken := request
	broken.ProfileID = "short"
	if err := store.PutRequest(ctx, broken); err == nil {
		t.Fatal("an invalid request was uploaded")
	}
}

// TestGooglePairingStoreRejectsMismatchedObjects covers the two ways a stored
// Drive object can disagree with its own name, plus the object-count guard.
func TestGooglePairingStoreRejectsMismatchedObjects(t *testing.T) {
	request := pairingStoreFixture(t)
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// The ID the store will look up is valid, but the object's name encodes a
	// different request.
	otherID := strings.Repeat("b", 32)

	var listing []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"files": listing})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/drive/v3/files/") {
			_, _ = writer.Write(payload)
			return
		}
		http.NotFound(writer, r)
	}))
	defer server.Close()
	originalAPI, originalUpload := googleDriveAPIBase, googleDriveUploadBase
	googleDriveAPIBase, googleDriveUploadBase = server.URL, server.URL
	t.Cleanup(func() {
		googleDriveAPIBase, googleDriveUploadBase = originalAPI, originalUpload
	})

	store := &googlePairingStore{remote: &googleDriveRemote{client: server.Client()}}
	listing = []map[string]string{{
		"id": "id-mismatch", "name": googlePairingObjectName("request", otherID),
	}}
	if _, err := store.ListRequests(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "does not match its id") {
		t.Fatalf("mismatched Drive object error = %v", err)
	}

	// A duplicate of the same object makes the lookup ambiguous.
	listing = []map[string]string{
		{"id": "id-1", "name": googlePairingObjectName("request", request.RequestID)},
		{"id": "id-2", "name": googlePairingObjectName("request", request.RequestID)},
	}
	if _, err := store.ListRequests(t.Context()); err != nil {
		t.Fatalf("listing duplicates: %v", err)
	}
	if _, _, err := store.GetResponse(
		t.Context(), request.RequestID,
	); err != nil && !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate lookup error = %v", err)
	}

	// More than 256 pending requests is refused before the objects are fetched.
	listing = make([]map[string]string, 0, 257)
	for i := range 257 {
		listing = append(listing, map[string]string{
			"id":   fmt.Sprintf("id-%d", i),
			"name": googlePairingObjectName("request", fmt.Sprintf("%032x", i)),
		})
	}
	if _, err := store.ListRequests(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "too many requests") {
		t.Fatalf("oversized Drive listing error = %v", err)
	}
}

// TestGooglePairingObjectNameValidation pins the naming rule shared by every
// Drive call, including that a valid request id under the wrong prefix is
// rejected rather than aliased onto the other namespace.
func TestGooglePairingObjectNameValidation(t *testing.T) {
	id := strings.Repeat("a", 32)
	for name, want := range map[string]bool{
		googlePairingObjectName("request", id):                 true,
		googlePairingObjectName("response", id):                true,
		"ccl-pair-request-" + id:                               false,
		"ccl-pair-request-short.ccl":                           false,
		"ccl-pair-other-" + id + ".ccl":                        false,
		"ccl-pair-request-" + strings.Repeat("Z", 32) + ".ccl": false,
		"": false,
	} {
		if got := validGooglePairingObjectName(name); got != want {
			t.Fatalf("validGooglePairingObjectName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestPairingStoreForManagerSelectsTheProviderBackend pins the provider switch
// and, for Google Drive, that an unauthorized remote fails before any network
// call is attempted.
func TestPairingStoreForManagerSelectsTheProviderBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localDir := filepath.Join(home, ".ccl")
	remoteDir := t.TempDir()
	remoteID := strings.Repeat("d", 32)

	store, err := pairingStoreForManager(t.Context(), &Manager{
		provider: providerICloud, remoteDir: remoteDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*filePairingStore); !ok {
		t.Fatalf("iCloud store = %T", store)
	}

	_, err = pairingStoreForManager(t.Context(), &Manager{
		localDir: localDir, remoteID: remoteID, alias: "drive",
		provider: providerGoogleDrive,
	})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("unauthorized Google Drive error = %v", err)
	}

	if _, err := pairingStoreForManager(t.Context(), &Manager{
		provider: "dropbox",
	}); err == nil || !strings.Contains(err.Error(), "does not support device pairing") {
		t.Fatalf("unsupported provider error = %v", err)
	}
}

// TestPairingStoreForPendingSelectsTheProviderBackend covers the pending-login
// path, where a Google Drive connection re-reads the bundle before pairing so
// the new device sees the current profile.
func TestPairingStoreForPendingSelectsTheProviderBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	remoteDir := t.TempDir()
	connection := pendingRemoteConnection{
		Version: registryVersion, Alias: "personal", Provider: providerICloud,
		RemoteID: strings.Repeat("e", 32), RemoteDir: remoteDir,
	}
	store, err := pairingStoreForPending(t.Context(), connection, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*filePairingStore); !ok {
		t.Fatalf("iCloud pending store = %T", store)
	}

	if _, err := pairingStoreForPending(t.Context(), pendingRemoteConnection{
		Provider: "dropbox",
	}, false); err == nil || !strings.Contains(err.Error(), "unsupported pairing provider") {
		t.Fatalf("unsupported pending provider error = %v", err)
	}

	// A Google Drive connection without a stored authorization fails the same
	// way the manager path does.
	if _, err := pairingStoreForPending(t.Context(), pendingRemoteConnection{
		Alias: "drive", Provider: providerGoogleDrive,
		AuthPath:  filepath.Join(home, ".ccl", googleAuthName),
		RemoteDir: filepath.Join(home, ".ccl", googleCacheName),
	}, false); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("unauthorized pending Google Drive error = %v", err)
	}
}

// TestPairingStoreForPendingRefreshesTheGoogleBundle pins the refresh flag: it
// downloads the encrypted bundle into the pending cache directory so the new
// device pairs against the current profile rather than a stale one.
func TestPairingStoreForPendingRefreshesTheGoogleBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localDir := filepath.Join(home, ".ccl")
	authPath := filepath.Join(localDir, googleAuthName)
	if err := saveGoogleToken(authPath, &oauth2.Token{
		AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	cacheDir, err := googleCacheDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// A bundle the refresh can download: one profile.json inside the zip.
	source := filepath.Join(t.TempDir(), googleCacheName)
	if err := os.MkdirAll(filepath.Join(source, snapshotsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(source, profileFileName),
		[]byte(`{"version":1,"id":"`+strings.Repeat("f", 32)+`","kdf":"master-key"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, verifierFileName), []byte("verifier"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := createCloudBundle(source, googleCacheName)
	if err != nil {
		t.Fatal(err)
	}

	downloaded := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/drive/v3/files":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"files":[{"id":"bundle","name":"`+
				googleBundleName+`","size":"1","version":"1"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/drive/v3/files/bundle":
			downloaded = true
			_, _ = writer.Write(bundle)
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

	store, err := pairingStoreForPending(t.Context(), pendingRemoteConnection{
		Version: registryVersion, Alias: "drive", Provider: providerGoogleDrive,
		RemoteID: strings.Repeat("f", 32), RemoteDir: cacheDir, AuthPath: authPath,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*googlePairingStore); !ok {
		t.Fatalf("pending Google Drive store = %T", store)
	}
	if !downloaded {
		t.Fatal("refresh did not download the bundle")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, profileFileName)); err != nil {
		t.Fatalf("refreshed cache missing profile: %v", err)
	}
}

// TestFilePairingStoreSortsRequestsByCreation pins the listing order the device
// list relies on when several requests are pending.
func TestFilePairingStoreSortsRequestsByCreation(t *testing.T) {
	remoteDir := t.TempDir()
	store, err := newFilePairingStore(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for i := range 3 {
		request := pairingStoreFixture(t)
		// Staggered creation times keep the requests inside their 10 minute
		// window while still making the sort order observable.
		request.CreatedAt = nowUTC().Add(-time.Duration(3-i) * time.Minute)
		request.ExpiresAt = request.CreatedAt.Add(pairingLifetime)
		if err := store.PutRequest(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	requests, err := store.ListRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("listed %d requests", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		if requests[i-1].CreatedAt.After(requests[i].CreatedAt) {
			t.Fatalf("requests are not sorted: %+v", requests)
		}
	}
}
