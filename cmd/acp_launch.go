package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"sync"

	"github.com/claude-code-launch/ccl/internal/claude"
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
	if cfg.ACPProvider != "" {
		p, ok := cfg.Providers[cfg.ACPProvider]
		if !ok {
			return acpProviderSnapshot{}, fmt.Errorf("provider %q selected for ACP was not found in configuration", cfg.ACPProvider)
		}
		return acpProviderSnapshot{name: cfg.ACPProvider, provider: cloneACPProvider(p)}, nil
	}

	p, err := resolveProviderFromEnvironment()
	if err != nil {
		return acpProviderSnapshot{}, fmt.Errorf("no provider selected for ACP. Use 'ccl use --acp <name>', or set OPENAI_API_KEY / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN in environment")
	}
	return acpProviderSnapshot{name: p.Name, provider: cloneACPProvider(p)}, nil
}

func cloneACPProvider(p provider.Provider) provider.Provider {
	cloned := p
	if p.Env != nil {
		cloned.Env = make(map[string]string, len(p.Env))
		for key, value := range p.Env {
			cloned.Env[key] = value
		}
	}
	if p.ModelOverrides != nil {
		cloned.ModelOverrides = make(map[string]string, len(p.ModelOverrides))
		for key, value := range p.ModelOverrides {
			cloned.ModelOverrides[key] = value
		}
	}
	if p.ModelProtocols != nil {
		cloned.ModelProtocols = make(map[string]string, len(p.ModelProtocols))
		for key, value := range p.ModelProtocols {
			cloned.ModelProtocols[key] = value
		}
	}
	return cloned
}

type acpPreparedLaunch interface {
	Command(args ...string) *exec.Cmd
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

type acpLaunchManager struct {
	mu             sync.Mutex
	resolve        func() (acpProviderSnapshot, error)
	prepareContext func(context.Context, provider.Provider) (acpPreparedLaunch, error)
	current        *acpLaunchGeneration
	generations    map[uint64]*acpLaunchGeneration
	nextID         uint64
	closed         bool
}

func newACPLaunchManager() *acpLaunchManager {
	return newACPLaunchManagerWithContext(resolveACPProvider, func(ctx context.Context, p provider.Provider) (acpPreparedLaunch, error) {
		return claude.PrepareContext(ctx, p)
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

func (m *acpLaunchManager) primeContext(ctx context.Context) error {
	lease, err := m.acquireContext(ctx)
	if err != nil {
		return err
	}
	lease.Release()
	return nil
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

func (l *acpLaunchLease) Command(cwd string, extra []string) *exec.Cmd {
	if l == nil || l.generation == nil || l.generation.launch == nil {
		return nil
	}
	cmd := l.generation.launch.Command(extra...)
	if cmd != nil {
		cmd.Dir = cwd
	}
	return cmd
}

func (l *acpLaunchLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.release(l.generation)
	})
}
