package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/claudeaccount"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

type acpProviderSnapshot struct {
	name     string
	provider provider.Provider
}

func resolveACPProvider() (acpProviderSnapshot, error) {
	cfg, err := config.Load()
	if err != nil {
		return acpProviderSnapshot{}, fmt.Errorf("failed to load config: %w", err)
	}
	name := cfg.EffectiveACPProvider()
	if provider.IsDefaultProvider(name) {
		if p, ok, err := cfg.ResolveDefault(); err != nil {
			return acpProviderSnapshot{}, err
		} else if ok {
			return acpProviderSnapshot{name: provider.DefaultProvider, provider: p.Clone()}, nil
		}
		return acpProviderSnapshot{name: provider.DefaultProvider, provider: provider.Provider{Name: provider.DefaultProvider}}, nil
	}
	if name == "" {
		return acpProviderSnapshot{}, fmt.Errorf("no provider selected for ACP. Use 'ccl use --acp <name>' ('default' runs Claude Code's own configuration), or 'ccl use <name>' (ACP follows it unless pinned)")
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return acpProviderSnapshot{}, fmt.Errorf("provider %q selected for ACP was not found in configuration", name)
	}
	return acpProviderSnapshot{name: name, provider: p.Clone()}, nil
}

type acpPreparedLaunch interface {
	Options(cwd string) (json.RawMessage, error)
	Close()
}

type acpLaunchGeneration struct {
	id       uint64
	snapshot acpProviderSnapshot
	launch   acpPreparedLaunch
	refs     int
	current  bool
	closed   bool
}

// acpSessionChoices are one ccl acp process's --model and --effort. They are
// the adapter's documented SDK options, so every session of that agent starts
// on them whichever provider is selected; the wrapper also selects the model
// through upstream's configuration interface so clients show it.
type acpSessionChoices struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

func (c acpSessionChoices) apply(options json.RawMessage) (json.RawMessage, error) {
	if c == (acpSessionChoices{}) {
		return options, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(options, &object); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{"model": c.Model, "effort": c.Effort} {
		if value != "" {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			object[key] = encoded
		}
	}
	return json.Marshal(object)
}

type acpLaunchManager struct {
	mu             sync.Mutex
	choices        acpSessionChoices
	resolve        func() (acpProviderSnapshot, error)
	prepareContext func(context.Context, provider.Provider) (acpPreparedLaunch, error)
	current        *acpLaunchGeneration
	generations    map[uint64]*acpLaunchGeneration
	nextID         uint64
	closed         bool
	profileBound   bool
	profileID      string
}

func newACPLaunchManager() *acpLaunchManager {
	return newACPLaunchManagerWithContext(resolveACPProvider, func(ctx context.Context, p provider.Provider) (acpPreparedLaunch, error) {
		if provider.IsDefaultProvider(p.Name) {
			return claude.DefaultAgentLaunch{}, nil
		}
		if provider.IsClaudeAccount(p) {
			store, err := claudeaccount.Open()
			if err != nil {
				return nil, err
			}
			profile, err := store.Profile(p.OAuthAccountCredential)
			if err != nil {
				return nil, err
			}
			return claude.ClaudeAccountAgentLaunch{ConfigDir: profile}, nil
		}
		return claude.PrepareAgentContext(ctx, p)
	})
}

func newACPLaunchManagerWith(
	resolve func() (acpProviderSnapshot, error),
	prepare func(provider.Provider) (acpPreparedLaunch, error),
) *acpLaunchManager {
	return newACPLaunchManagerWithContext(resolve, func(_ context.Context, p provider.Provider) (acpPreparedLaunch, error) {
		return prepare(p)
	})
}

func newACPLaunchManagerWithContext(
	resolve func() (acpProviderSnapshot, error),
	prepare func(context.Context, provider.Provider) (acpPreparedLaunch, error),
) *acpLaunchManager {
	return &acpLaunchManager{
		resolve:        resolve,
		prepareContext: prepare,
		generations:    make(map[uint64]*acpLaunchGeneration),
	}
}

func (m *acpLaunchManager) acquire() (*acpLaunchLease, error) {
	return m.acquireContext(context.Background())
}

func (m *acpLaunchManager) acquireContext(ctx context.Context) (*acpLaunchLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("ACP launch manager is closed")
	}

	snapshot, err := m.resolve()
	if err != nil {
		return nil, err
	}
	if m.profileBound {
		id := ""
		if provider.IsClaudeAccount(snapshot.provider) {
			id = snapshot.provider.OAuthAccountCredential
		}
		if id != m.profileID {
			return nil, fmt.Errorf("Claude account profile changed; restart ccl acp to use the selected provider")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.current != nil && equalACPProviderSnapshot(m.current.snapshot, snapshot) {
		m.current.refs++
		return newACPLaunchLease(m, m.current), nil
	}

	launch, err := m.prepareContext(ctx, snapshot.provider)
	if err != nil {
		if launch != nil {
			launch.Close()
		}
		return nil, err
	}
	if launch == nil {
		return nil, fmt.Errorf("prepare ACP launch returned nil")
	}
	if err := ctx.Err(); err != nil {
		launch.Close()
		return nil, err
	}
	m.nextID++
	next := &acpLaunchGeneration{
		id:       m.nextID,
		snapshot: snapshot,
		launch:   launch,
		refs:     1,
		current:  true,
	}
	m.generations[next.id] = next
	previous := m.current
	m.current = next
	if previous != nil {
		previous.current = false
		m.closeGenerationIfUnusedLocked(previous)
	}
	return newACPLaunchLease(m, next), nil
}

// Upstream stores session metadata in its process-wide Claude config dir. Do
// not let a running ACP process silently resume through another account.
func (m *acpLaunchManager) bindProfile(l *acpLaunchLease) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profileBound = true
	if provider.IsClaudeAccount(l.generation.snapshot.provider) {
		m.profileID = l.generation.snapshot.provider.OAuthAccountCredential
	}
	return m.profileID
}

func equalACPProviderSnapshot(left, right acpProviderSnapshot) bool {
	return left.name == right.name && reflect.DeepEqual(left.provider, right.provider)
}

func (m *acpLaunchManager) release(generation *acpLaunchGeneration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation == nil || generation.closed || generation.refs == 0 {
		return
	}
	generation.refs--
	m.closeGenerationIfUnusedLocked(generation)
}

func (m *acpLaunchManager) closeGenerationIfUnusedLocked(generation *acpLaunchGeneration) {
	if generation == nil || generation.closed || generation.current || generation.refs != 0 {
		return
	}
	generation.closed = true
	delete(m.generations, generation.id)
	generation.launch.Close()
}

func (m *acpLaunchManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	if m.current != nil {
		m.current.current = false
		m.current = nil
	}
	for _, generation := range m.generations {
		m.closeGenerationIfUnusedLocked(generation)
	}
}

type acpLaunchLease struct {
	manager    *acpLaunchManager
	generation *acpLaunchGeneration
	once       sync.Once
}

func newACPLaunchLease(manager *acpLaunchManager, generation *acpLaunchGeneration) *acpLaunchLease {
	return &acpLaunchLease{manager: manager, generation: generation}
}

func (l *acpLaunchLease) Generation() uint64 {
	if l == nil || l.generation == nil {
		return 0
	}
	return l.generation.id
}

// Default reports whether the lease runs Claude Code's own configuration.
func (l *acpLaunchLease) Default() bool {
	return l != nil && l.generation != nil && provider.IsDefaultProvider(l.generation.snapshot.name)
}

func (l *acpLaunchLease) Options(cwd string) (json.RawMessage, error) {
	if l == nil || l.generation == nil || l.generation.launch == nil {
		return nil, fmt.Errorf("ACP launch is not available")
	}
	options, err := l.generation.launch.Options(cwd)
	if err != nil {
		return nil, err
	}
	return l.manager.choices.apply(options)
}

func (l *acpLaunchLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.release(l.generation)
	})
}
