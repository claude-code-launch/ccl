package claudeaccount

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProfilesRemainIndependentAndSelectable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, name := range []string{"work", "personal"} {
		candidate, err := store.Prepare(name)
		if err != nil || !candidate.New {
			t.Fatalf("prepare %s: %+v %v", name, candidate, err)
		}
		if err := os.WriteFile(filepath.Join(candidate.Path, ".credentials.json"), []byte("fake-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, candidate.Path)
	}
	if paths[0] == paths[1] {
		t.Fatal("two accounts share a profile")
	}
	name, path, err := store.Resolve("")
	if err != nil || name != "personal" || path != paths[1] {
		t.Fatalf("selected profile = %q %q %v", name, path, err)
	}
	if err := store.Select(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	name, path, err = store.Resolve("")
	if err != nil || name != "work" || path != paths[0] {
		t.Fatalf("switched profile = %q %q %v", name, path, err)
	}
	again, err := store.Prepare("work")
	if err != nil || again.New || again.Path != paths[0] {
		t.Fatalf("re-authentication did not reuse profile: %+v %v", again, err)
	}
	names, active, err := store.List()
	if err != nil || !reflect.DeepEqual(names, []string{"personal", "work"}) || active != "work" {
		t.Fatalf("accounts = %v, %q, %v", names, active, err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ccl", "claude-accounts", "accounts.json"))
	if err != nil || strings.Contains(string(data), "fake-") {
		t.Fatalf("registry contains credentials or is unreadable: %v", err)
	}
	if err := store.Select(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	name, path, err = store.Resolve("")
	if err != nil || name != "" || path != "" {
		t.Fatalf("default = %q %q %v", name, path, err)
	}
	if err := store.Select(context.Background(), "missing"); err == nil {
		t.Fatal("missing account was selected")
	}
	generated, err := store.Prepare("")
	if err != nil {
		t.Fatal(err)
	}
	generated = store.PreferName(generated, "user@example.invalid")
	if generated.Name != "user@example.invalid" {
		t.Fatalf("unnamed account was not given its reported email: %q", generated.Name)
	}
	if err := store.Activate(context.Background(), generated); err != nil {
		t.Fatal(err)
	}
	other, err := store.Prepare("")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.PreferName(other, "user@example.invalid"); got.Name == "user@example.invalid" {
		t.Fatal("a new account could replace an existing email alias")
	}
}

func TestRegistryRejectsUntrustedPathsAndNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store, _ := Open()
	for _, name := range []string{"default", "list", "use", "broken\nname"} {
		if _, err := store.Prepare(name); err == nil {
			t.Fatalf("accepted reserved or invalid account %q", name)
		}
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "accounts.json"), []byte(`{"accounts":{"work":"../../elsewhere"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve("work"); err == nil {
		t.Fatal("registry path traversal was accepted")
	}
}

func TestAccountEnvironmentRemovesOtherAuthentication(t *testing.T) {
	got := Environment([]string{
		"PATH=/bin", "CLAUDE_CONFIG_DIR=/old", "claude_securestorage_config_dir=/old-creds",
		"ANTHROPIC_API_KEY=fake", "ANTHROPIC_AUTH_TOKEN=fake", "ANTHROPIC_BASE_URL=https://old.invalid",
		"ANTHROPIC_MODEL=old-model", "CLAUDE_CODE_OAUTH_TOKEN=fake", "CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_HOST_CREDS_FILE=/old-creds", "CLAUDE_CODE_HOST_AUTH_ENV_VAR=HOST_TOKEN",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
		"CLAUDE_CODE_USE_BUILTIN_RIPGREP=0", "UNRELATED=keep",
	}, "/new profile")
	want := []string{"PATH=/bin", "CLAUDE_CODE_USE_BUILTIN_RIPGREP=0", "UNRELATED=keep", "CLAUDE_CONFIG_DIR=/new profile"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("account environment = %q, want %q", got, want)
	}
}
