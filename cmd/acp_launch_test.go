package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestClaudeAccountACPProfileBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store, _ := claudeaccount.Open()
	work, err := store.PrepareNew("work")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.PrepareNew("other")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &provider.Config{ActiveProvider: "work", Providers: map[string]provider.Provider{
		"work": claudeAccountProvider("work", work.ID()), "other": claudeAccountProvider("other", other.ID()),
	}}
	if err = config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager := newACPLaunchManager()
	defer manager.Close()
	lease, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if got := manager.bindProfile(lease); got != work.ID() {
		t.Fatalf("bound profile %q", got)
	}
	raw, err := lease.Options(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var options struct {
		Env      map[string]string `json:"env"`
		Settings map[string]any    `json:"settings"`
	}
	if err = json.Unmarshal(raw, &options); err != nil {
		t.Fatal(err)
	}
	badProject := t.TempDir()
	if err := os.Mkdir(filepath.Join(badProject, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badProject, ".claude", "settings.json"), []byte(`{"apiKeyHelper":"echo fake-helper"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Options(badProject); err == nil || !strings.Contains(err.Error(), "apiKeyHelper") {
		t.Fatalf("ACP account accepted conflicting project authentication: %v", err)
	}
	if options.Env["CLAUDE_CONFIG_DIR"] != work.Path || options.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != work.Path || options.Env["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("account SDK options: %+v", options.Env)
	}
	if err = config.Update(func(c *provider.Config) error {
		c.ActiveProvider = "default"
		c.DefaultClaudeProvider = "work"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	same, err := manager.acquire()
	if err != nil {
		t.Fatalf("default should resolve to the bound account: %v", err)
	}
	same.Release()
	if err = config.Update(func(c *provider.Config) error { c.DefaultClaudeProvider = "other"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.acquire(); err == nil || !strings.Contains(err.Error(), "restart ccl acp") {
		t.Fatalf("account changed without restart: %v", err)
	}
}

type fakeACPPreparedLaunch struct {
	closed atomic.Int32
}

func (l *fakeACPPreparedLaunch) Options(cwd string) (json.RawMessage, error) {
	return json.RawMessage(`{"settings":{},"env":{}}`), nil
}

func (l *fakeACPPreparedLaunch) Close() {
	l.closed.Add(1)
}

func TestResolveACPProviderIsIndependentFromActiveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &provider.Config{
		ActiveProvider: "alpha",
		ACPProvider:    "beta",
		Providers: map[string]provider.Provider{
			"alpha": {Name: "alpha", Endpoint: "https://alpha.test"},
			"beta":  {Name: "beta", Endpoint: "https://beta.test"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got, err := resolveACPProvider()
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "beta" || got.provider.Endpoint != "https://beta.test" {
		t.Fatalf("resolved ACP provider = %+v", got)
	}
	cfg.ActiveProvider = "beta"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err = resolveACPProvider()
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "beta" || got.provider.Endpoint != "https://beta.test" {
		t.Fatalf("active provider change affected ACP resolution: %+v", got)
	}
}

func TestACPLaunchManagerGenerationsAndLeases(t *testing.T) {
	var mu sync.Mutex
	snapshot := acpProviderSnapshot{name: "a", provider: provider.Provider{Name: "a", Endpoint: "https://a.test", Model: "one"}}
	var launches []*fakeACPPreparedLaunch
	manager := newACPLaunchManagerWith(func() (acpProviderSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		return acpProviderSnapshot{name: snapshot.name, provider: snapshot.provider.Clone()}, nil
	}, func(provider.Provider) (acpPreparedLaunch, error) {
		launch := &fakeACPPreparedLaunch{}
		launches = append(launches, launch)
		return launch, nil
	})

	first, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation() != second.Generation() || len(launches) != 1 {
		t.Fatalf("unchanged snapshot created another generation: ids %d/%d launches=%d", first.Generation(), second.Generation(), len(launches))
	}

	mu.Lock()
	snapshot.provider.Model = "two"
	mu.Unlock()
	third, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if third.Generation() == first.Generation() || len(launches) != 2 {
		t.Fatalf("provider update did not create a generation: ids %d/%d launches=%d", first.Generation(), third.Generation(), len(launches))
	}
	if got := launches[0].closed.Load(); got != 0 {
		t.Fatalf("old launch closed with live leases: %d", got)
	}
	first.Release()
	if got := launches[0].closed.Load(); got != 0 {
		t.Fatalf("old launch closed before final lease: %d", got)
	}
	second.Release()
	second.Release()
	if got := launches[0].closed.Load(); got != 1 {
		t.Fatalf("old launch close count = %d, want 1", got)
	}
	third.Release()
	if got := launches[1].closed.Load(); got != 0 {
		t.Fatalf("current launch closed while manager is live: %d", got)
	}
	manager.Close()
	manager.Close()
	if got := launches[1].closed.Load(); got != 1 {
		t.Fatalf("current launch close count = %d, want 1", got)
	}
}

func TestACPLaunchManagerPrepareFailureKeepsCurrentGeneration(t *testing.T) {
	snapshot := acpProviderSnapshot{name: "a", provider: provider.Provider{Name: "a"}}
	fail := false
	var launches []*fakeACPPreparedLaunch
	manager := newACPLaunchManagerWith(func() (acpProviderSnapshot, error) {
		return snapshot, nil
	}, func(provider.Provider) (acpPreparedLaunch, error) {
		if fail {
			return nil, errors.New("prepare failed")
		}
		launch := &fakeACPPreparedLaunch{}
		launches = append(launches, launch)
		return launch, nil
	})
	defer manager.Close()

	first, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	snapshot = acpProviderSnapshot{name: "b", provider: provider.Provider{Name: "b"}}
	fail = true
	if _, err := manager.acquire(); err == nil {
		t.Fatal("expected prepare failure")
	}
	if got := launches[0].closed.Load(); got != 0 {
		t.Fatalf("failed replacement closed current launch: %d", got)
	}
	fail = false
	second, err := manager.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation() == first.Generation() {
		t.Fatal("successful retry reused stale generation")
	}
	if got := launches[0].closed.Load(); got != 1 {
		t.Fatalf("replaced launch close count = %d, want 1", got)
	}
	second.Release()
}

func TestACPLaunchManagerAcquireHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := newACPLaunchManagerWithContext(func() (acpProviderSnapshot, error) {
		return acpProviderSnapshot{name: "a", provider: provider.Provider{Name: "a"}}, nil
	}, func(context.Context, provider.Provider) (acpPreparedLaunch, error) {
		t.Fatal("prepare should not run for a canceled context")
		return nil, nil
	})
	defer manager.Close()
	if _, err := manager.acquireContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want context.Canceled", err)
	}
}

func TestACPLaunchManagerConcurrentAcquirePreparesOnce(t *testing.T) {
	var prepares atomic.Int32
	manager := newACPLaunchManagerWith(func() (acpProviderSnapshot, error) {
		return acpProviderSnapshot{name: "a", provider: provider.Provider{Name: "a"}}, nil
	}, func(provider.Provider) (acpPreparedLaunch, error) {
		prepares.Add(1)
		time.Sleep(10 * time.Millisecond)
		return &fakeACPPreparedLaunch{}, nil
	})
	defer manager.Close()

	const count = 16
	leases := make([]*acpLaunchLease, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			leases[index], errs[index] = manager.acquire()
		}(i)
	}
	wg.Wait()
	if got := prepares.Load(); got != 1 {
		t.Fatalf("prepare count = %d, want 1", got)
	}
	generation := leases[0].Generation()
	for i := range count {
		if errs[i] != nil {
			t.Fatalf("acquire %d: %v", i, errs[i])
		}
		if leases[i].Generation() != generation {
			t.Fatalf("generation %d = %d, want %d", i, leases[i].Generation(), generation)
		}
		leases[i].Release()
	}
}

// TestACPFollowsTheActiveProviderUntilPinned pins S9 end to end: unpinned ACP
// tracks ccl use, a pin holds, and --follow releases it.
func TestACPFollowsTheActiveProviderUntilPinned(t *testing.T) {
	seedProviders(t, "a", "a", "b")
	resolve := func() string {
		t.Helper()
		snapshot, err := resolveACPProvider()
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.name
	}
	if got := resolve(); got != "a" {
		t.Fatalf("unpinned ACP = %q, want the active provider", got)
	}
	if err := runProviderUse("b", false); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != "b" {
		t.Fatalf("unpinned ACP did not follow ccl use: %q", got)
	}
	if err := runProviderUse("a", true); err != nil {
		t.Fatal(err)
	}
	if err := runProviderUse("b", false); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != "a" {
		t.Fatalf("pinned ACP moved with ccl use: %q", got)
	}
	if err := runACPFollow(); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != "b" {
		t.Fatalf("--follow did not release the pin: %q", got)
	}

	// Nothing selected anywhere: a clear error, never a shell-derived provider.
	seedProviders(t, "", "a")
	t.Setenv("OPENAI_API_KEY", "sk-other-tool")
	if _, err := resolveACPProvider(); err == nil {
		t.Fatal("ACP resolved a provider with nothing selected")
	}
}

func TestParseUseFollowRequiresACP(t *testing.T) {
	if _, _, follow, _, err := parseProviderUseArgs([]string{"--acp", "--follow"}); err != nil || !follow {
		t.Fatalf("--acp --follow = %t, %v", follow, err)
	}
	for _, args := range [][]string{{"--follow"}, {"--acp", "--follow", "x"}} {
		if _, _, _, _, err := parseProviderUseArgs(args); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}
