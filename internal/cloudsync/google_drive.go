package cloudsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
)

var (
	googleDriveAPIBase    = "https://www.googleapis.com"
	googleDriveUploadBase = "https://www.googleapis.com"
)

type googleDriveRemote struct {
	client                *http.Client
	bundleObserved        bool
	expectedBundleID      string
	expectedBundleVersion string
}

type googleDriveFile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Size    string `json:"size"`
	Version string `json:"version"`
}

func newGoogleDriveRemote(ctx context.Context, token *oauth2.Token, authPath string) *googleDriveRemote {
	config := googleOAuthConfig("")
	source := &savingTokenSource{
		source:   config.TokenSource(ctx, token),
		authPath: authPath,
		last:     token,
	}
	return &googleDriveRemote{client: oauth2.NewClient(ctx, source)}
}

func googleCacheDirectory() (string, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(localDir, googleCacheName), nil
}

func (remote *googleDriveRemote) downloadBundle(cacheDir string) (bool, error) {
	file, found, err := remote.findBundle()
	if err != nil {
		return false, err
	}
	remote.bundleObserved = true
	remote.expectedBundleID = ""
	remote.expectedBundleVersion = ""
	if !found {
		if err := replaceGoogleCache(cacheDir, nil); err != nil {
			return false, err
		}
		return false, nil
	}
	remote.expectedBundleID = file.ID
	remote.expectedBundleVersion = file.Version
	request, err := http.NewRequest(http.MethodGet,
		googleDriveAPIBase+"/drive/v3/files/"+url.PathEscape(file.ID)+"?alt=media", nil)
	if err != nil {
		return false, err
	}
	response, err := remote.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("download encrypted Google Drive sync bundle: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, googleDriveResponseError("download sync bundle", response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBundleSize+1))
	if err != nil {
		return false, fmt.Errorf("read Google Drive sync bundle: %w", err)
	}
	if len(data) > maxBundleSize {
		return false, fmt.Errorf("Google Drive sync bundle exceeds the %d MiB safety limit", maxBundleSize>>20)
	}
	if err := replaceGoogleCache(cacheDir, data); err != nil {
		return false, err
	}
	return true, nil
}

func (remote *googleDriveRemote) uploadBundle(cacheDir string) error {
	data, err := createGoogleBundle(cacheDir)
	if err != nil {
		return err
	}
	file, found, err := remote.findBundle()
	if err != nil {
		return err
	}
	if remote.bundleObserved {
		switch {
		case remote.expectedBundleID == "" && found:
			return fmt.Errorf("Google Drive sync bundle was created by another device; refresh before pushing")
		case remote.expectedBundleID != "" && !found:
			return fmt.Errorf("Google Drive sync bundle was deleted by another device; refresh before pushing")
		case found && (file.ID != remote.expectedBundleID ||
			(remote.expectedBundleVersion != "" && file.Version != remote.expectedBundleVersion)):
			return fmt.Errorf("Google Drive sync bundle changed after it was downloaded; pull or refresh before pushing")
		}
	}
	if found {
		request, requestErr := http.NewRequest(
			http.MethodPatch,
			googleDriveUploadBase+"/upload/drive/v3/files/"+url.PathEscape(file.ID)+"?uploadType=media&fields=id,version",
			bytes.NewReader(data),
		)
		if requestErr != nil {
			return requestErr
		}
		request.Header.Set("Content-Type", "application/octet-stream")
		response, doErr := remote.client.Do(request)
		if doErr != nil {
			return fmt.Errorf("upload encrypted Google Drive sync bundle: %w", doErr)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return googleDriveResponseError("update sync bundle", response)
		}
		updated, err := decodeGoogleDriveFileResponse(response.Body)
		if err != nil {
			return err
		}
		remote.bundleObserved = true
		remote.expectedBundleID = file.ID
		if updated.ID != "" {
			remote.expectedBundleID = updated.ID
		}
		if updated.Version != "" {
			remote.expectedBundleVersion = updated.Version
		}
		return nil
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataHeader := make(textproto.MIMEHeader)
	metadataHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return err
	}
	metadata := struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}{Name: googleBundleName, Parents: []string{"appDataFolder"}}
	if err := json.NewEncoder(metadataPart).Encode(metadata); err != nil {
		return err
	}
	mediaHeader := make(textproto.MIMEHeader)
	mediaHeader.Set("Content-Type", "application/octet-stream")
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return err
	}
	if _, err := mediaPart.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	request, err := http.NewRequest(
		http.MethodPost,
		googleDriveUploadBase+"/upload/drive/v3/files?uploadType=multipart&fields=id,version",
		&body,
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())
	response, err := remote.client.Do(request)
	if err != nil {
		return fmt.Errorf("upload encrypted Google Drive sync bundle: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return googleDriveResponseError("create sync bundle", response)
	}
	created, err := decodeGoogleDriveFileResponse(response.Body)
	if err != nil {
		return err
	}
	remote.bundleObserved = true
	remote.expectedBundleID = created.ID
	remote.expectedBundleVersion = created.Version
	return nil
}

func (remote *googleDriveRemote) findBundle() (googleDriveFile, bool, error) {
	query := url.Values{}
	query.Set("spaces", "appDataFolder")
	query.Set("pageSize", "10")
	query.Set("fields", "files(id,name,size,version)")
	query.Set("q", "name = '"+googleBundleName+"' and 'appDataFolder' in parents")
	request, err := http.NewRequest(http.MethodGet,
		googleDriveAPIBase+"/drive/v3/files?"+query.Encode(), nil)
	if err != nil {
		return googleDriveFile{}, false, err
	}
	response, err := remote.client.Do(request)
	if err != nil {
		return googleDriveFile{}, false, fmt.Errorf("list Google Drive application data: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return googleDriveFile{}, false, googleDriveResponseError("list application data", response)
	}
	var result struct {
		Files []googleDriveFile `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return googleDriveFile{}, false, fmt.Errorf("decode Google Drive file list: %w", err)
	}
	var matches []googleDriveFile
	for _, file := range result.Files {
		if file.Name == googleBundleName && file.ID != "" {
			matches = append(matches, file)
		}
	}
	switch len(matches) {
	case 0:
		return googleDriveFile{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return googleDriveFile{}, false, fmt.Errorf("Google Drive contains multiple %s files; remove duplicates before syncing", googleBundleName)
	}
}

func decodeGoogleDriveFileResponse(body io.Reader) (googleDriveFile, error) {
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return googleDriveFile{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return googleDriveFile{}, nil
	}
	var file googleDriveFile
	if err := json.Unmarshal(data, &file); err != nil {
		return googleDriveFile{}, fmt.Errorf("decode Google Drive file metadata: %w", err)
	}
	return file, nil
}

func (remote *googleDriveRemote) deleteAllApplicationData(ctx context.Context) error {
	files, err := remote.listApplicationFiles(ctx)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.Name != googleBundleName && !strings.HasPrefix(file.Name, "ccl-pair-") {
			continue
		}
		request, err := http.NewRequestWithContext(
			ctx, http.MethodDelete,
			googleDriveAPIBase+"/drive/v3/files/"+url.PathEscape(file.ID),
			nil,
		)
		if err != nil {
			return err
		}
		response, err := remote.client.Do(request)
		if err != nil {
			return fmt.Errorf("delete Google Drive application data: %w", err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = googleDriveResponseError("delete application data", response)
			_ = response.Body.Close()
			return err
		}
		if err := response.Body.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (remote *googleDriveRemote) listApplicationFiles(ctx context.Context) ([]googleDriveFile, error) {
	query := url.Values{}
	query.Set("spaces", "appDataFolder")
	query.Set("pageSize", "1000")
	query.Set("fields", "files(id,name,size,version)")
	query.Set("q", "'appDataFolder' in parents")
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, googleDriveAPIBase+"/drive/v3/files?"+query.Encode(), nil,
	)
	if err != nil {
		return nil, err
	}
	response, err := remote.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list Google Drive application data: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, googleDriveResponseError("list application data", response)
	}
	var result struct {
		Files []googleDriveFile `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode Google Drive file list: %w", err)
	}
	return result.Files, nil
}

func googleDriveResponseError(operation string, response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	var apiError struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	message := ""
	if json.Unmarshal(data, &apiError) == nil {
		message = strings.TrimSpace(apiError.Error.Message)
		if apiError.Error.Status != "" {
			message = apiError.Error.Status + ": " + message
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("Google Drive %s failed (%s): %s", operation, response.Status, message)
}
