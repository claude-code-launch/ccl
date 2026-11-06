package modelsdev

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func cacheTestCatalog(efforts string) string {
	return fmt.Sprintf(`{"p":{"api":"https://example.invalid/v1","models":{"model":{"reasoning":true,"reasoning_options":[{"type":"effort","values":%s}]}}}}`, efforts)
}

func TestModelCacheRefreshUsesETagAndReloadsChangedIndex(t *testing.T) {
	body, etag := cacheTestCatalog(`["low","high"]`), `"one"`
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("metadata request carried account credentials")
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	cache := &Cache{Path: filepath.Join(t.TempDir(), "modelsdev.json"), URL: server.URL, Client: server.Client()}
	snapshot, err := cache.Refresh(context.Background())
	if err != nil || snapshot.Offline || len(snapshot.Providers) != 1 {
		t.Fatalf("first refresh: %v %+v", err, snapshot)
	}
	first, err := cache.Index()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(cache.Path)
	snapshot, err = cache.Refresh(context.Background())
	if err != nil || snapshot.Offline || requests != 2 {
		t.Fatalf("conditional refresh: %v %+v requests=%d", err, snapshot, requests)
	}
	after, _ := os.Stat(cache.Path)
	again, _ := cache.Index()
	if first != again || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("304 rebuilt the index or rewrote the snapshot")
	}
	body, etag = cacheTestCatalog(`["low","high","max"]`), `"two"`
	if _, err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := cache.Index()
	if err != nil {
		t.Fatal(err)
	}
	efforts, _ := updated.Efforts("p", "model", "")
	if updated == first || !slices.Equal(efforts, []string{"low", "high", "max"}) {
		t.Fatalf("changed snapshot did not replace the index: %v", efforts)
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0o600 {
		t.Fatal("cache permissions are not private")
	}
}

func TestModelCacheKeepsLastGoodSnapshotOnRefreshFailures(t *testing.T) {
	for _, failure := range []string{"http", "invalid-json", "unusable-models", "unreachable"} {
		t.Run(failure, func(t *testing.T) {
			old := []byte(cacheTestCatalog(`["low","high"]`))
			path := filepath.Join(t.TempDir(), "modelsdev.json")
			if err := os.WriteFile(path, old, 0o600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch failure {
				case "http":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "invalid-json":
					_, _ = w.Write([]byte(`{"p":`))
				default:
					_, _ = w.Write([]byte(`{"p":{"models":{}}}`))
				}
			}))
			defer server.Close()
			if failure == "unreachable" {
				server.Close()
			}
			cache := &Cache{Path: path, URL: server.URL, Client: server.Client()}
			snapshot, err := cache.Refresh(context.Background())
			if err != nil || !snapshot.Offline || len(snapshot.Providers) != 1 {
				t.Fatalf("offline fallback: %v %+v", err, snapshot)
			}
			preserved, _ := os.ReadFile(path)
			if !bytes.Equal(preserved, old) {
				t.Fatal("failed refresh changed the last good snapshot")
			}
		})
	}
}

func TestModelCacheRejects304WithoutCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) }))
	defer server.Close()
	cache := &Cache{Path: filepath.Join(t.TempDir(), "modelsdev.json"), URL: server.URL}
	if _, err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("304 invented an offline snapshot")
	}
	if _, err := os.Stat(cache.Path); !os.IsNotExist(err) {
		t.Fatal("failed refresh created a cache file")
	}
}

func TestModelCacheValidatorMustMatchSnapshotContents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			t.Error("validator for another snapshot was sent")
		}
		_, _ = w.Write([]byte(cacheTestCatalog(`["high"]`)))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(path, []byte(cacheTestCatalog(`["low"]`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".meta.json", []byte(`{"etag":"old","sha256":"mismatched"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Cache{Path: path, URL: server.URL}).Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestModelCacheMissingOrCorruptIndexIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modelsdev.json")
	cache := &Cache{Path: path}
	if _, err := cache.Index(); !os.IsNotExist(err) {
		t.Fatalf("missing snapshot: %v", err)
	}
	if err := os.WriteFile(path, []byte(`invalid`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Index(); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("corrupt snapshot: %v", err)
	}
}
