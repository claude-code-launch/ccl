package modelsdev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

// Snapshot is a validated catalog fetched during configuration. Offline means
// refresh failed but the last valid local snapshot remains usable.
type Snapshot struct {
	Providers map[string]Provider
	Offline   bool
}

// Cache downloads public metadata only; it never receives provider credentials.
// URL and Client can point to local services in isolated tests.
type Cache struct {
	Path   string
	URL    string
	Client *http.Client
}

type cacheMetadata struct {
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"`
}

// CachePath is separate from provider configuration and OAuth credentials.
func CachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ccl", "cache", "modelsdev.json"), nil
}

// CacheStamp lets an explicitly requested proxy start notice a metadata
// refresh without reading/decoding the catalog or changing provider config.
func CacheStamp() string {
	path, err := CachePath()
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// Refresh updates the on-disk snapshot, or returns the last valid offline copy.
// Call from OAuth / ccl set, never from a model request.
func Refresh(ctx context.Context) (*Snapshot, error) {
	path, err := CachePath()
	if err != nil {
		return nil, err
	}
	return (&Cache{Path: path, URL: apiURL}).Refresh(ctx)
}

// CachedIndex is read-only and performs no network access. A cached immutable
// index is reused until the snapshot file changes (for example via ccl set).
func CachedIndex() (*Index, error) {
	path, err := CachePath()
	if err != nil {
		return nil, err
	}
	return (&Cache{Path: path}).Index()
}

var indexCache = struct {
	sync.Mutex
	files map[string]cachedIndex
}{files: make(map[string]cachedIndex)}

type cachedIndex struct {
	modified time.Time
	size     int64
	index    *Index
}

func (c *Cache) Index() (*Index, error) {
	indexCache.Lock()
	defer indexCache.Unlock()
	file, err := os.Open(c.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if cached, ok := indexCache.files[c.Path]; ok && cached.size == info.Size() && cached.modified.Equal(info.ModTime()) {
		return cached.index, nil
	}
	data, err := readCatalogBody(file)
	if err != nil {
		return nil, err
	}
	index, err := newIndex(data)
	if err != nil {
		return nil, err
	}
	indexCache.files[c.Path] = cachedIndex{index: index, size: info.Size(), modified: info.ModTime()}
	return index, nil
}

func (c *Cache) Refresh(ctx context.Context) (*Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A writer lock serializes HTTP validators with atomic snapshot replacement
	// across processes. Failed updates never truncate the last usable file.
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return c.offline(err)
	}
	release, err := fsutil.AcquireFileLock(ctx, c.Path+".lock")
	if err != nil {
		return c.offline(err)
	}
	defer release()

	cached, _ := os.ReadFile(c.Path)
	var metadata cacheMetadata
	if data, err := os.ReadFile(c.Path + ".meta.json"); err == nil {
		_ = json.Unmarshal(data, &metadata)
	}
	if metadata.SHA256 != catalogHash(cached) {
		metadata.ETag = ""
	}
	endpoint := c.URL
	if endpoint == "" {
		endpoint = defaultAPIURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return c.offline(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccl")
	if len(cached) > 0 && metadata.ETag != "" {
		request.Header.Set("If-None-Match", metadata.ETag)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
		defer client.CloseIdleConnections()
	}
	response, err := client.Do(request)
	if err != nil {
		return c.offline(fmt.Errorf("refresh models.dev metadata: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return decodeSnapshot(cached)
	}
	if response.StatusCode != http.StatusOK {
		return c.offline(fmt.Errorf("refresh models.dev metadata: HTTP %d", response.StatusCode))
	}
	data, err := readCatalogBody(response.Body)
	if err != nil {
		return c.offline(err)
	}
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return c.offline(err)
	}
	if catalogHash(data) != catalogHash(cached) {
		if err := fsutil.WriteFileAtomic(c.Path, data, 0o600); err != nil {
			return c.offline(err)
		}
	}
	metadata = cacheMetadata{ETag: response.Header.Get("ETag"), SHA256: catalogHash(data)}
	encoded, _ := json.Marshal(metadata)
	// The validator is optional. Its hash prevents a crash between these two
	// atomic writes from associating an ETag with different snapshot contents.
	_ = fsutil.WriteFileAtomic(c.Path+".meta.json", encoded, 0o600)
	return snapshot, nil
}

func (c *Cache) offline(refreshErr error) (*Snapshot, error) {
	data, err := os.ReadFile(c.Path)
	if err == nil {
		var snapshot *Snapshot
		snapshot, err = decodeSnapshot(data)
		if err == nil {
			snapshot.Offline = true
			return snapshot, nil
		}
	}
	return nil, fmt.Errorf("models.dev metadata unavailable (no valid offline cache): %w", refreshErr)
}

func decodeSnapshot(data []byte) (*Snapshot, error) {
	if _, err := newIndex(data); err != nil {
		return nil, err
	}
	providers, err := decodeCatalog(data)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Providers: providers}, nil
}

func catalogHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func readCatalogBody(reader io.Reader) ([]byte, error) {
	const limit = 64 << 20
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("models.dev metadata exceeds size limit")
	}
	return data, nil
}
