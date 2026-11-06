package proxymanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/codex"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

type catalogRefresh struct {
	configStamp   string
	initialized   bool
	providers     []provider.Provider
	runtimeHash   string
	lastDiscovery time.Time
	done          <-chan struct{}
}

type reloadRequest struct {
	Provider   string   `json:"provider"`
	Additional []string `json:"additional,omitempty"`
	result     chan error
}

func boundProviders(primary provider.Provider, additional []string) ([]provider.Provider, error) {
	result := []provider.Provider{primary}
	if len(additional) == 0 {
		return result, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("read CCL providers failed")
	}
	seen := map[string]bool{primary.Name: true}
	for _, name := range additional {
		if seen[name] {
			continue
		}
		seen[name] = true
		p, exists := cfg.Providers[name]
		if !exists || provider.IsClaudeAccount(p) || provider.IsDefaultProvider(name) {
			return nil, fmt.Errorf("provider %q cannot be attached to Codex", name)
		}
		p.Name = name
		result = append(result, p)
	}
	return result, nil
}

func providersHash(providers []provider.Provider) string {
	if len(providers) == 1 {
		return providerHash(providers[0])
	}
	var key strings.Builder
	for _, p := range providers {
		key.WriteString(providerHash(p))
		key.WriteByte('\n')
	}
	return providerHash(provider.Provider{Model: key.String()})
}

func runtimeProvidersHash(providers []provider.Provider) string {
	runtimeProviders := make([]provider.Provider, len(providers))
	for i, p := range providers {
		p.ContextPreset = ""
		p.EffortLevel = ""
		p.ModelsDevProvider = ""
		p.Env = maps.Clone(p.Env)
		for _, key := range provider.ManagedContextEnvKeys() {
			delete(p.Env, key)
		}
		for _, slot := range []*string{&p.Model, &p.CustomModelID, &p.OpusModel, &p.SonnetModel, &p.HaikuModel, &p.FableModel, &p.SubagentModel} {
			*slot = strings.ReplaceAll(*slot, "[1m]", "")
		}
		if len(p.Env) == 0 {
			p.Env = nil
		}
		runtimeProviders[i] = p
	}
	return providersHash(runtimeProviders)
}

func prepareBindings(ctx context.Context, providers []provider.Provider) ([]codex.RuntimeBinding, <-chan struct{}, func(), error) {
	bindings := make([]codex.RuntimeBinding, 0, len(providers))
	cleanup := func() {
		for _, binding := range bindings {
			binding.Close()
		}
	}
	done := make(chan struct{})
	var once sync.Once
	for _, p := range providers {
		session, err := providersession.PrepareProxy(ctx, p)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("prepare Codex provider %q failed: %w", p.Name, err)
		}
		bindings = append(bindings, codex.RuntimeBinding{Provider: p, Runtime: session.Runtime, Close: session.Close})
		go func(session *providersession.Session) { <-session.Runtime.Done(); once.Do(func() { close(done) }) }(session)
	}
	return bindings, done, cleanup, nil
}

func (r *catalogRefresh) update(ctx context.Context, m *Manager, gateway *codex.Gateway, state *State, force bool, selected string, additional []string) error {
	var stamp string
	if info, err := os.Stat(config.ConfigPath()); err == nil {
		stamp = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	metadataStamp := modelsdev.CacheStamp()
	discoveryDue := false
	for _, p := range r.providers {
		if p.OAuthProvider == "gemini" && time.Since(r.lastDiscovery) >= 5*time.Minute {
			discoveryDue = true
		}
	}
	if !force && r.initialized && stamp == r.configStamp && metadataStamp == state.MetadataStamp && !discoveryDue {
		return nil
	}
	latest := r.providers
	primary := state.Provider
	names := state.AdditionalProviders
	if selected != "" {
		primary = selected
		names = additional
	}
	if stamp != "" {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("read CCL configuration for catalog refresh failed")
		}
		found, exists := cfg.Providers[primary]
		if !exists {
			return fmt.Errorf("bound Codex provider %q is missing; retaining the last registry", primary)
		}
		found.Name = primary
		latest, err = boundProviders(found, names)
		if err != nil {
			return err
		}
	}
	if len(latest) == 0 {
		return fmt.Errorf("no bound Codex providers")
	}
	updated := *state
	updated.Provider = primary
	updated.AdditionalProviders = append([]string(nil), names...)
	updated.ConfigHash = providersHash(latest)
	updated.MetadataStamp = metadataStamp
	// Roll back the catalog file if journaling fails. No new route is published
	// until both the owned catalog and recovery state have been persisted.
	persist := func(models []codex.Model, replace bool) error {
		before, err := os.ReadFile(state.Codex.CatalogPath)
		if err != nil {
			return err
		}
		if replace {
			err = state.Codex.ReplaceCatalog(models)
		} else {
			err = state.Codex.RefreshCatalog(models)
		}
		if err != nil {
			return err
		}
		nextOverlay, selectionErr := state.Codex.PlanModelSelection(models)
		if selectionErr != nil {
			_ = fsutil.WriteFileAtomic(state.Codex.CatalogPath, before, 0600)
			return selectionErr
		}
		updated.Codex = nextOverlay
		if err = writeJSON(m.statePath(), &updated); err != nil {
			if rollbackErr := fsutil.WriteFileAtomic(state.Codex.CatalogPath, before, 0600); rollbackErr != nil {
				return fmt.Errorf("catalog journal and rollback failed")
			}
			return err
		}
		if err := nextOverlay.ApplyModelSelection(ctx, state.Codex); err != nil {
			_ = fsutil.WriteFileAtomic(state.Codex.CatalogPath, before, 0600)
			_ = writeJSON(m.statePath(), state)
			return err
		}
		return nil
	}
	nextRuntimeHash := runtimeProvidersHash(latest)
	if force || discoveryDue || nextRuntimeHash != r.runtimeHash {
		bindings, done, cleanup, err := prepareBindings(ctx, latest)
		if err != nil {
			return err
		}
		// A failed rediscovery must not withdraw the running account's last good
		// inventory, even when the saved pool predates newly enabled models.
		for _, binding := range bindings {
			for _, previous := range r.providers {
				sameAccount := previous.Name == binding.Provider.Name && previous.OAuthProvider == binding.Provider.OAuthProvider && previous.OAuthAccountCredential == binding.Provider.OAuthAccountCredential
				if sameAccount && binding.Provider.OAuthProvider != "" && (binding.Runtime.ModelCatalogIsFallback() || binding.Runtime.ModelCatalogIsStale()) {
					cleanup()
					r.lastDiscovery = time.Now()
					return fmt.Errorf("account model discovery unavailable; retaining the last registry")
				}
			}
		}
		if err = gateway.ReplaceProviders(bindings, func(models []codex.Model) error { return persist(models, true) }); err != nil {
			cleanup()
			return err
		}
		r.done = done
		r.runtimeHash = nextRuntimeHash
		r.lastDiscovery = time.Now()
	} else {
		if err := gateway.RefreshProvidersMetadata(latest, func(models []codex.Model) error { return persist(models, false) }); err != nil {
			return err
		}
	}
	*state = updated
	r.providers = latest
	r.configStamp = stamp
	r.initialized = true
	return nil
}

func reloadControl(ctx context.Context, state *State, primary string, additional []string) error {
	// The validated health probe prevents credentials from being sent to a
	// tampered control endpoint; redirects remain disabled for the actual reload.
	if err := control(ctx, state, "GET", "/health"); err != nil {
		return err
	}
	raw, err := json.Marshal(reloadRequest{Provider: primary, Additional: additional})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, state.ControlURL+"/reload", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || string(body) != state.ID {
		return fmt.Errorf("Codex reload failed; previous registry retained")
	}
	return nil
}
