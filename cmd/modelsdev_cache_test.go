package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
)

func TestOAuthMetadataRefreshHappensAfterSaveAndIsNonfatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previousLogin, previousRefresh := oauthLogin, modelsDevRefresh
	t.Cleanup(func() { oauthLogin, modelsDevRefresh = previousLogin, previousRefresh })
	oauthLogin = func(context.Context, string, oauthproxy.LoginOptions) (oauthproxy.LoginResult, error) {
		return oauthproxy.LoginResult{Provider: "workbuddycn", Path: "bound-account.json"}, nil
	}
	called := false
	modelsDevRefresh = func(ctx context.Context) (*modelsdev.Snapshot, error) {
		called = true
		if _, ok := ctx.Deadline(); !ok {
			t.Error("metadata refresh has no deadline")
		}
		cfg, err := config.Load()
		if err != nil || cfg.Providers["work"].OAuthProvider != "workbuddycn" {
			t.Error("metadata refresh ran before OAuth provider was saved")
		}
		return nil, errors.New("offline")
	}
	var out bytes.Buffer
	if err := runAuth(context.Background(), &out, []string{"workbuddycn", "work"}, authOptions{}); err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(out.String(), "compatibility settings") {
		t.Fatalf("metadata failure blocked/vanished: %s", out.String())
	}
}

func TestModelsDevProviderPickerUsesOfflineSnapshot(t *testing.T) {
	previous := modelsDevRefresh
	t.Cleanup(func() { modelsDevRefresh = previous })
	modelsDevRefresh = func(context.Context) (*modelsdev.Snapshot, error) {
		return &modelsdev.Snapshot{Offline: true, Providers: map[string]modelsdev.Provider{"b": {ID: "b", Name: "Beta"}, "a": {ID: "a", Name: "Alpha"}}}, nil
	}
	providers, err := modelsDevProviders(context.Background())
	if err != nil || len(providers) != 2 || providers[0].ID != "a" {
		t.Fatalf("offline provider picker: %v %+v", err, providers)
	}
	var out bytes.Buffer
	if err := refreshModelsDevMetadata(context.Background(), &out); err != nil || !strings.Contains(out.String(), "offline model metadata cache") {
		t.Fatalf("offline refresh: %v %s", err, out.String())
	}
}

func TestSetRefreshModelsDoesNotOpenEditorOrSaveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := modelsDevRefresh
	previousOut := setCmd.OutOrStdout()
	t.Cleanup(func() {
		modelsDevRefresh = previous
		setCmd.SetOut(previousOut)
		_ = setCmd.Flags().Set("refresh-models", "false")
	})
	called := false
	modelsDevRefresh = func(context.Context) (*modelsdev.Snapshot, error) { called = true; return &modelsdev.Snapshot{}, nil }
	var out bytes.Buffer
	setCmd.SetOut(&out)
	_ = setCmd.Flags().Set("refresh-models", "true")
	if err := setCmd.RunE(setCmd, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil || !called || len(cfg.Providers) != 0 {
		t.Fatalf("metadata refresh changed providers: %v %+v", err, cfg)
	}
}
