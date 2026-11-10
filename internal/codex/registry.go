package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// RuntimeBinding gives each provider an explicit account/runtime owner. The
// gateway takes ownership of Close only after a successful publication.
type RuntimeBinding struct {
	Provider provider.Provider
	Runtime  *oauthproxy.Runtime
	Close    func()
}

type runtimeGeneration struct {
	active  int // guarded by Gateway.catalogMu
	retired bool
	close   func()
}

type catalogSnapshot struct {
	models           []Model
	routes           map[string]Route
	runtimes         map[string]*oauthproxy.Runtime
	legacyCompaction map[string]Route
	native           Catalog
	generation       *runtimeGeneration
}

type runtimeRequestKey struct{}

func (g *Gateway) requestRuntime(r *http.Request) *oauthproxy.Runtime {
	if runtime, ok := r.Context().Value(runtimeRequestKey{}).(*oauthproxy.Runtime); ok && runtime != nil {
		return runtime
	}
	return g.runtime
}

func (g *Gateway) currentLocked() *catalogSnapshot {
	if snapshot := g.modelSnapshot.Load(); snapshot != nil {
		return snapshot
	}
	if g.generation == nil {
		g.generation = &runtimeGeneration{}
	}
	runtimes := make(map[string]*oauthproxy.Runtime, len(g.routes))
	for id := range g.routes {
		runtimes[id] = g.runtime
	}
	return &catalogSnapshot{models: g.models, routes: g.routes, runtimes: runtimes, legacyCompaction: g.legacyCompaction, native: g.nativeCatalog, generation: g.generation}
}

func (g *Gateway) OwnRuntime(close func()) {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	g.currentLocked().generation.close = close
}

func retireGeneration(gen *runtimeGeneration) func() {
	gen.retired = true
	if gen.active == 0 {
		close := gen.close
		gen.close = nil
		return close
	}
	return nil
}

func (g *Gateway) acquireSnapshot() (*catalogSnapshot, func()) {
	g.catalogMu.Lock()
	snapshot := g.currentLocked()
	snapshot.generation.active++
	g.catalogMu.Unlock()
	return snapshot, func() {
		g.catalogMu.Lock()
		snapshot.generation.active--
		var close func()
		if snapshot.generation.retired && snapshot.generation.active == 0 {
			close = snapshot.generation.close
			snapshot.generation.close = nil
		}
		g.catalogMu.Unlock()
		if close != nil {
			close()
		}
	}
}

// ReplaceProviders atomically publishes a complete route set after persistence.
// In-flight requests lease the previous immutable snapshot until their bodies
// (including streaming responses) finish. Port, token and histories stay put.
func (g *Gateway) ReplaceProviders(bindings []RuntimeBinding, persist func([]Model) error) error {
	if len(bindings) == 0 {
		return fmt.Errorf("a Codex registry needs at least one provider")
	}
	next := &catalogSnapshot{routes: make(map[string]Route), runtimes: make(map[string]*oauthproxy.Runtime), generation: &runtimeGeneration{}}
	names := make(map[string]bool)
	var cleanups []func()
	for _, binding := range bindings {
		p := binding.Provider
		if binding.Runtime == nil || p.Name == "" || names[p.Name] || provider.ValidateRuntime(p) != nil {
			return fmt.Errorf("invalid or duplicate Codex runtime binding")
		}
		names[p.Name] = true
		prepared := &Gateway{routes: make(map[string]Route)}
		if err := prepared.buildModels(binding.Runtime, p); err != nil {
			return err
		}
		for alias, route := range prepared.routes {
			next.routes[alias] = route
			next.runtimes[alias] = binding.Runtime
		}
		next.models = append(next.models, prepared.models...)
		if binding.Close != nil {
			cleanups = append(cleanups, binding.Close)
		}
	}
	// Distinguish identical model labels from different accounts only when
	// necessary; single-provider picker labels retain their existing names.
	labelCounts := make(map[string]int)
	for _, model := range next.models {
		label, _ := model["display_name"].(string)
		labelCounts[strings.ToLower(label)]++
	}
	for _, model := range next.models {
		label, _ := model["display_name"].(string)
		if labelCounts[strings.ToLower(label)] > 1 {
			alias, _ := model["slug"].(string)
			model["display_name"] = label + " (" + next.routes[alias].Provider + ")"
		}
	}
	next.generation.close = func() {
		for _, close := range cleanups {
			close()
		}
	}
	g.catalogMu.Lock()
	old := g.currentLocked()
	next.native = old.native
	applyClientToolMode(next.native, next.models, next.routes)
	next.legacyCompaction = make(map[string]Route, len(old.legacyCompaction))
	defaultRoute := next.routes[next.models[0]["slug"].(string)]
	for id, route := range old.legacyCompaction {
		if replacement, ok := next.routes[route.Alias]; ok {
			next.legacyCompaction[id] = replacement
		} else {
			next.legacyCompaction[id] = defaultRoute
		}
	}
	if err := persist(cloneCatalogModels(next.models)); err != nil {
		g.catalogMu.Unlock()
		return err
	}
	g.modelSnapshot.Store(next)
	closeOld := retireGeneration(old.generation)
	g.catalogMu.Unlock()
	if closeOld != nil {
		closeOld()
	}
	return nil
}

func (g *Gateway) serveCatalog(w http.ResponseWriter, r *http.Request, snapshot *catalogSnapshot) {
	catalog := Catalog{Models: cloneCatalogModels(snapshot.models)}
	if !g.options.Independent {
		var err error
		catalog, err = MergeCatalog(snapshot.native, catalog.Models)
		if err != nil {
			writeError(w, 500, "api_error", "cannot build Codex catalog", "")
			return
		}
	}
	raw, err := json.Marshal(catalog)
	if err != nil || len(raw) > 1<<20 {
		writeError(w, 500, "api_error", "Codex remote catalog exceeds 1 MiB", "")
		return
	}
	hash := sha256.Sum256(append(append([]byte(nil), raw...), []byte(r.URL.Query().Get("client_version"))...))
	etag := `"` + hex.EncodeToString(hash[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization, ChatGPT-Account-Id")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// RefreshProvidersMetadata keeps each route bound to its existing account.
func (g *Gateway) RefreshProvidersMetadata(providers []provider.Provider, persist func([]Model) error) error {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	current := g.currentLocked()
	next := *current
	next.models = cloneCatalogModels(current.models)
	for _, p := range providers {
		var runtime *oauthproxy.Runtime
		for alias, route := range current.routes {
			if route.Provider == p.Name {
				runtime = current.runtimes[alias]
				break
			}
		}
		if runtime == nil {
			continue
		}
		details := runtime.ModelDetails()
		if _, err := modelsdev.CachedIndex(); err == nil {
			details, _ = withModelsDevReasoning(runtime.Models(), details, runtime.ModelDisplayNames(), p)
		} else {
			details = nil
		}
		for _, model := range next.models {
			alias, _ := model["slug"].(string)
			route, ok := current.routes[alias]
			if !ok || route.Provider != p.Name {
				continue
			}
			applyProviderContext(model, provider.StripContextSuffix(route.Model), p)
			info, ok := details[route.Model]
			if !ok {
				info = details[strings.ToLower(route.Model)]
			}
			if !route.NativeResponses && info.ReasoningKnown {
				model["supported_reasoning_levels"], model["default_reasoning_level"] = reasoningLevels(info)
			}
		}
	}
	if err := persist(next.models); err != nil {
		return err
	}
	g.modelSnapshot.Store(&next)
	return nil
}

func (g *Gateway) finishCatalogPlan() {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	if current := g.modelSnapshot.Load(); current != nil {
		next := *current
		next.native = g.nativeCatalog
		next.legacyCompaction = maps.Clone(g.legacyCompaction)
		g.modelSnapshot.Store(&next)
	}
}
