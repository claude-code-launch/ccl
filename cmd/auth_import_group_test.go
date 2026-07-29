package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestAuthImportDirectoryIsOneLevelAndCreatesProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.json":   `{"type":"xai","access_token":"a","email":"a@example.com"}`,
		"b.json":   `{"type":"xai","access_token":"b","email":"b@example.com"}`,
		"note.txt": `{"type":"xai","access_token":"ignored","email":"ignored@example.com"}`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "c.json"), []byte(`{"type":"xai","access_token":"c","email":"c@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAuthImport(&out, source, authImportOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"grok-a@example.com", "grok-b@example.com"} {
		p, ok := cfg.Providers[name]
		if !ok {
			t.Fatalf("missing imported provider %q: %+v", name, cfg.Providers)
		}
		if p.OAuthProvider != "grok" || !strings.HasPrefix(p.OAuthAccountCredential, "xai-") {
			t.Fatalf("imported provider %q = %+v", name, p)
		}
	}
	if _, ok := cfg.Providers["grok-c@example.com"]; ok {
		t.Fatal("directory import recursed into nested directory")
	}
	authEntries, err := os.ReadDir(filepath.Join(home, ".ccl", "auth"))
	if err != nil {
		t.Fatal(err)
	}
	if len(authEntries) != 2 {
		t.Fatalf("managed auth entries = %d, want 2", len(authEntries))
	}
}

func TestAuthGroupGeneratedProviderAndSyncPrunesDeletedCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := t.TempDir()
	for _, email := range []string{"a@example.com", "b@example.com"} {
		data := `{"type":"xai","access_token":"token-` + email + `","email":"` + email + `"}`
		if err := os.WriteFile(filepath.Join(source, email+".json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runAuthImport(&bytes.Buffer{}, source, authImportOptions{}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAuthGroupEdit(&out, "grok1", authGroupOptions{
		oauthProvider: "grok",
		members:       "grok-a@example.com,grok-b@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	group := cfg.AuthGroups["grok1"]
	if group.OAuthProvider != "grok" || len(group.Credentials) != 2 {
		t.Fatalf("group = %+v", group)
	}
	groupProvider := cfg.Providers["grok1"]
	if groupProvider.AuthGroup != "grok1" || groupProvider.OAuthProvider != "grok" {
		t.Fatalf("generated provider = %+v", groupProvider)
	}
	if len(groupProvider.OAuthAccountCredentials) != 2 {
		t.Fatalf("hydrated runtime credentials = %+v", groupProvider.OAuthAccountCredentials)
	}
	if groupProvider.CustomModelID != "grok-4.5" {
		t.Fatalf("Grok model defaults missing: %+v", groupProvider)
	}

	cfg.ActiveProvider = "grok1"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(home, ".ccl", "auth", "xai-a@example.com.json")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	result, err := syncAuthConfigWithOptions(nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.GroupsPruned != 1 {
		t.Fatalf("sync result = %+v", result)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["grok-a@example.com"]; ok {
		t.Fatal("provider for deleted credential survived sync")
	}
	if cfg.ActiveProvider != "grok1" {
		t.Fatalf("active group changed during sync: %q", cfg.ActiveProvider)
	}
	if got := cfg.AuthGroups["grok1"].Credentials; len(got) != 1 || got[0] != "xai-b@example.com.json" {
		t.Fatalf("pruned group credentials = %+v", got)
	}
	if got := cfg.Providers["grok1"].OAuthAccountCredentials; len(got) != 1 || got[0] != "xai-b@example.com.json" {
		t.Fatalf("hydrated group provider credentials = %+v", got)
	}
}

func TestAuthGroupCopyMoveRemovePreservesSharedMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"source": {
				Name:          "source",
				OAuthProvider: "grok",
				AuthGroup:     "source",
				SonnetModel:   "custom-shared-model",
			},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"source": {OAuthProvider: "grok", Credentials: []string{"xai-a.json"}},
		},
		ActiveProvider: "source",
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	if err := runAuthGroupCopy(&bytes.Buffer{}, "source", "copy"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["copy"].SonnetModel != "custom-shared-model" {
		t.Fatalf("copied mapping = %+v", loaded.Providers["copy"])
	}
	if err := runAuthGroupMove(&bytes.Buffer{}, "copy", "production"); err != nil {
		t.Fatal(err)
	}
	loaded, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.AuthGroups["copy"]; ok {
		t.Fatal("old copied group survived move")
	}
	if loaded.Providers["production"].AuthGroup != "production" {
		t.Fatalf("moved provider = %+v", loaded.Providers["production"])
	}
	if err := runAuthGroupRemove(&bytes.Buffer{}, "source", true); err != nil {
		t.Fatal(err)
	}
	loaded, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveProvider != "" {
		t.Fatalf("active provider after group removal = %q", loaded.ActiveProvider)
	}
	if _, ok := loaded.Providers["source"]; ok {
		t.Fatal("removed group provider survived")
	}
}

func TestConfigLoadHydratesEmptyGroupWithoutBackendLeak(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"empty": {Name: "empty", OAuthProvider: "grok", AuthGroup: "empty"},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"empty": {OAuthProvider: "grok", Credentials: []string{}},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	credentials := loaded.Providers["empty"].OAuthAccountCredentials
	if credentials == nil || len(credentials) != 0 {
		t.Fatalf("empty group hydration = %#v; want non-nil empty slice", credentials)
	}
}

func TestSyncPreservesCustomGroupProviderName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "xai-a.json"),
		[]byte(`{"type":"xai","access_token":"a","email":"a@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"grok-pool": {
				Name:          "grok-pool",
				OAuthProvider: "grok",
				AuthGroup:     "team",
			},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"team": {OAuthProvider: "grok", Credentials: []string{"xai-a.json"}},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := syncAuthConfigWithOptions(nil, false, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Providers["grok-pool"]; !ok {
		t.Fatalf("custom group provider was removed: %+v", loaded.Providers)
	}
	if _, ok := loaded.Providers["group-team"]; ok {
		t.Fatalf("sync recreated prefix-based duplicate: %+v", loaded.Providers)
	}
}

func TestAuthGroupCanCreateCustomProviderName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(source,
		[]byte(`{"type":"xai","access_token":"a","email":"a@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runAuthImport(&bytes.Buffer{}, source, authImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := runAuthGroupEdit(&bytes.Buffer{}, "team", authGroupOptions{
		oauthProvider: "grok",
		members:       "grok-a@example.com",
		providerName:  "grok-pool",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["grok-pool"]; got.AuthGroup != "team" {
		t.Fatalf("custom group provider = %+v", got)
	}
	if _, exists := cfg.Providers["team"]; exists {
		t.Fatalf("unexpected prefix-based provider: %+v", cfg.Providers)
	}
}

func TestDoctorValidatesGroupCredentialTypes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"xai-a.json":   `{"type":"xai","access_token":"a"}`,
		"codex-b.json": `{"type":"codex","access_token":"b"}`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &provider.Config{
		AuthGroups: map[string]provider.AuthGroup{
			"team": {
				OAuthProvider: "grok",
				Credentials:   []string{"xai-a.json", "codex-b.json"},
			},
		},
	}
	p := provider.Provider{
		Name:          "anything",
		OAuthProvider: "grok",
		AuthGroup:     "team",
	}
	result := validateDoctorAuthGroup(cfg, p)
	if result.AvailableMembers != 1 {
		t.Fatalf("available members = %d, want 1: %+v", result.AvailableMembers, result)
	}
	if len(result.Problems) != 1 || !strings.Contains(result.Problems[0], `has type "codex"`) {
		t.Fatalf("doctor problems = %+v", result.Problems)
	}

	cfg.AuthGroups["team"] = provider.AuthGroup{
		OAuthProvider: "grok",
		Credentials:   []string{"xai-a.json"},
	}
	result = validateDoctorAuthGroup(cfg, p)
	if len(result.Problems) != 0 || result.AvailableMembers != 1 {
		t.Fatalf("valid group rejected: %+v", result)
	}
}

func TestChooseAuthGroupNameSelectsExistingOrCreatesNew(t *testing.T) {
	originalSelect := authGroupSelectPrompt
	originalName := authGroupNamePrompt
	t.Cleanup(func() {
		authGroupSelectPrompt = originalSelect
		authGroupNamePrompt = originalName
	})
	cfg := &provider.Config{
		ActiveProvider: "grok-pool",
		Providers: map[string]provider.Provider{
			"grok-pool": {Name: "grok-pool", AuthGroup: "team"},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"team": {OAuthProvider: "grok", Credentials: []string{"xai-a.json", "xai-b.json"}},
		},
	}

	authGroupSelectPrompt = func(_ string, items []string) (string, error) {
		if len(items) != 2 || !strings.Contains(items[0], "Create new") {
			t.Fatalf("chooser items = %+v", items)
		}
		if !strings.Contains(items[1], "team") || !strings.Contains(items[1], "grok-pool") ||
			!strings.Contains(items[1], "(active)") {
			t.Fatalf("existing group label = %q", items[1])
		}
		return items[1], nil
	}
	authGroupNamePrompt = func(_, _ string) (string, error) {
		t.Fatal("name prompt should not run for an existing group")
		return "", nil
	}
	name, selected, err := chooseAuthGroupName(cfg)
	if err != nil || !selected || name != "team" {
		t.Fatalf("existing selection = %q, %t, %v", name, selected, err)
	}

	authGroupSelectPrompt = func(_ string, items []string) (string, error) {
		return items[0], nil
	}
	authGroupNamePrompt = func(_, placeholder string) (string, error) {
		if placeholder != "grok1" {
			t.Fatalf("name placeholder = %q", placeholder)
		}
		return "new-team", nil
	}
	name, selected, err = chooseAuthGroupName(cfg)
	if err != nil || !selected || name != "new-team" {
		t.Fatalf("new selection = %q, %t, %v", name, selected, err)
	}
}

func TestAuthGroupListShowsConfigurationAndCredentialPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &provider.Config{
		Providers: map[string]provider.Provider{
			"grok-pool": {Name: "grok-pool", AuthGroup: "team", OAuthProvider: "grok"},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"team": {
				OAuthProvider: "grok",
				Credentials:   []string{"xai-b.json", "xai-a.json"},
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAuthGroupList(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Group configuration:",
		"Name : config.yaml",
		"Path : " + filepath.Join(home, ".ccl", "config.yaml"),
		"team",
		"Provider    : grok-pool",
		"Backend     : grok",
		"- " + filepath.Join(home, ".ccl", "auth", "xai-a.json"),
		"- " + filepath.Join(home, ".ccl", "auth", "xai-b.json"),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("group list should contain %q, got:\n%s", want, text)
		}
	}
	if strings.Index(text, "xai-a.json") > strings.Index(text, "xai-b.json") {
		t.Fatalf("credential names should be sorted, got:\n%s", text)
	}
}

func TestDoctorGroupHealthCounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"xai-ok.json":      `{"type":"xai","access_token":"a"}`,
		"xai-quota.json":   `{"type":"xai","access_token":"b","status_message":"quota exhausted","quota":{"exceeded":true}}`,
		"xai-bad.json":     `{"type":"xai","access_token":"c","disabled":true}`,
		"xai-unavail.json": `{"type":"xai","access_token":"d","unavailable":true}`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &provider.Config{
		AuthGroups: map[string]provider.AuthGroup{
			"team": {
				OAuthProvider: "grok",
				Credentials:   []string{"xai-ok.json", "xai-quota.json", "xai-bad.json", "xai-unavail.json", "missing.json"},
			},
		},
	}
	p := provider.Provider{Name: "gg", OAuthProvider: "grok", AuthGroup: "team"}
	result := validateDoctorAuthGroup(cfg, p)
	if result.HealthyMembers != 1 || result.QuotaMembers != 1 || result.InvalidMembers != 3 {
		t.Fatalf("health counts = healthy=%d quota=%d invalid=%d problems=%v",
			result.HealthyMembers, result.QuotaMembers, result.InvalidMembers, result.Problems)
	}
}

func TestMaskAPIKey(t *testing.T) {
	if got := maskAPIKey(""); got != "(unset)" {
		t.Fatalf("empty = %q", got)
	}
	if got := maskAPIKey("abcd1234efgh5678"); !strings.HasPrefix(got, "abcd") || !strings.HasSuffix(got, "5678") {
		t.Fatalf("masked = %q", got)
	}
}

func TestSyncCleansInvalidCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".ccl", "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"xai-good.json":  `{"type":"xai","access_token":"g","email":"good@example.com"}`,
		"xai-bad.json":   `{"type":"xai","access_token":"b","email":"bad@example.com","unavailable":true,"status":"error"}`,
		"xai-quota.json": `{"type":"xai","access_token":"q","email":"quota@example.com","quota":{"exceeded":true},"status_message":"quota exhausted"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &provider.Config{
		ActiveProvider: "pool",
		Providers: map[string]provider.Provider{
			"pool": {Name: "pool", OAuthProvider: "grok", AuthGroup: "team", Type: "openai", Endpoint: "oauth://xai"},
		},
		AuthGroups: map[string]provider.AuthGroup{
			"team": {OAuthProvider: "grok", Credentials: []string{"xai-good.json", "xai-bad.json", "xai-quota.json"}},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	result, err := syncAuthConfigWithOptions(nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cleaned != 1 {
		t.Fatalf("cleaned = %d, want 1 (unavailable only); deleted=%v", result.Cleaned, result.DeletedFiles)
	}
	if _, err := os.Stat(filepath.Join(authDir, "xai-bad.json")); !os.IsNotExist(err) {
		t.Fatalf("unavailable file should be deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "xai-quota.json")); err != nil {
		t.Fatalf("quota file should remain by default: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "xai-good.json")); err != nil {
		t.Fatalf("good file missing: %v", err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	members := loaded.AuthGroups["team"].Credentials
	if len(members) != 2 {
		t.Fatalf("group members after clean = %v", members)
	}

	result, err = syncAuthConfigWithOptions(nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cleaned != 1 {
		t.Fatalf("quota clean = %d, want 1; deleted=%v", result.Cleaned, result.DeletedFiles)
	}
	if _, err := os.Stat(filepath.Join(authDir, "xai-quota.json")); !os.IsNotExist(err) {
		t.Fatalf("quota file should be deleted with --clean-quota: %v", err)
	}
}
