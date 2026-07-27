package oauthproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestEnrichAndHydrateAuthHealthRoundTrip(t *testing.T) {
	auth := &coreauth.Auth{
		Provider:      "xai",
		Status:        coreauth.StatusError,
		StatusMessage: "quota exhausted",
		Unavailable:   true,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
			BackoffLevel:  2,
		},
		NextRetryAfter: time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second),
		Metadata: map[string]any{
			"type":         "xai",
			"access_token": "tok",
		},
	}
	enrichAuthMetadataForPersist(auth)
	if !auth.Metadata["unavailable"].(bool) {
		t.Fatal("unavailable not mirrored")
	}
	q, ok := auth.Metadata["quota"].(map[string]any)
	if !ok || q["exceeded"] != true {
		t.Fatalf("quota metadata = %#v", auth.Metadata["quota"])
	}

	reloaded := &coreauth.Auth{
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Metadata: auth.Metadata,
	}
	hydrateAuthHealthFromMetadata(reloaded)
	if !reloaded.Unavailable || !reloaded.Quota.Exceeded || reloaded.StatusMessage != "quota exhausted" {
		t.Fatalf("hydrated = %+v quota=%+v", reloaded, reloaded.Quota)
	}
}

func TestProviderTokenStorePersistsQuotaMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := "xai-user@example.com.json"
	rawIn := `{"type":"xai","access_token":"a","refresh_token":"r","email":"user@example.com"}`
	if err := os.WriteFile(filepath.Join(authDir, file), []byte(rawIn), 0o600); err != nil {
		t.Fatal(err)
	}

	store := newProviderTokenStoreResolver(authDir, "xai", []string{file}, true, nil)
	auths, err := store.List(context.Background())
	if err != nil || len(auths) != 1 {
		t.Fatalf("list = %v, %v", auths, err)
	}
	auth := auths[0]
	auth.Unavailable = true
	auth.Status = coreauth.StatusError
	auth.StatusMessage = "quota exhausted"
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "quota"
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(authDir, file))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["unavailable"] != true {
		t.Fatalf("file metadata = %#v", meta)
	}
	q, _ := meta["quota"].(map[string]any)
	if q == nil || q["exceeded"] != true {
		t.Fatalf("file quota = %#v", meta["quota"])
	}

	_, info, err := parseCredential(raw, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if !info.QuotaExceeded || !info.Unavailable {
		t.Fatalf("parsed info = %+v", info)
	}
}
