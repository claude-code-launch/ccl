package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/groklaunch"
	"github.com/claude-code-launch/ccl/internal/modelsdev"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/claude-code-launch/ccl/internal/providersession"
)

// apiMaskSuffix is how many trailing characters of an API key /api/config
// returns. The key itself never leaves the machine.
const apiMaskSuffix = 4

// ConfigView is the JSON shape of ccl's own configuration.
type ConfigView struct {
	Active    ActiveView              `json:"active"`
	Lang      string                  `json:"lang"`
	Bypass    bool                    `json:"bypassMode"`
	LogLevel  string                  `json:"logLevel"`
	Default   DefaultView             `json:"default"`
	Providers map[string]ProviderView `json:"providers"`
	Order     []string                `json:"order"`
}

// ActiveView is the persisted active provider selection.
type ActiveView struct {
	Normal string `json:"normal"`
}

// DefaultView describes the reserved `default` selector. It is never a key of
// the providers map, cannot be edited, and always exists.
type DefaultView struct {
	Name         string   `json:"name"`
	IsDefault    bool     `json:"isDefault"`
	Active       bool     `json:"active"`
	EffortLevels []string `json:"effortLevels,omitempty"`
	// LegacyClaude is non-empty when an upgraded config still remembers a
	// retired Claude default account and needs an explicit switch to `default`.
	LegacyClaude string `json:"legacyClaude,omitempty"`
}

// ProviderView is one provider with its credential masked.
type ProviderView struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Endpoint      string `json:"endpoint"`
	APIKey        string `json:"apiKey"`
	HasKey        bool   `json:"hasKey"`
	OAuthProvider string `json:"oauthProvider,omitempty"`
	EffortLevel   string `json:"effortLevel,omitempty"`
	Models        string `json:"models,omitempty"`
	DefaultModel  string `json:"defaultModel,omitempty"`
	FastMode      bool   `json:"fastMode,omitempty"`
	ContextPreset string `json:"contextPreset,omitempty"`
	// ContextWindow is the window a Balanced preset caps every model to; zero
	// for Default, where a model's [1m] marker decides.
	ContextWindow int  `json:"contextWindow,omitempty"`
	Active        bool `json:"active"`
	// ModelChoices is what a conversation on this provider can pick.
	ModelChoices []ModelChoice `json:"modelChoices"`
}

// ProviderInput is the writable subset of a provider. Its fields are pointers
// so PATCH can distinguish "leave alone" (nil) from "clear" ("").
type ProviderInput struct {
	Name          *string `json:"name"`
	Type          *string `json:"type"`
	Endpoint      *string `json:"endpoint"`
	APIKey        *string `json:"apiKey"`
	OAuthProvider *string `json:"oauthProvider"`
	EffortLevel   *string `json:"effortLevel"`
	Models        *string `json:"models"`
	DefaultModel  *string `json:"defaultModel"`
	FastMode      *bool   `json:"fastMode"`
	ContextPreset *string `json:"contextPreset"`
}

var errNoProvider = fmt.Errorf("provider not found")

// handleConfig serves GET /api/config and PUT /api/config.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	case "PUT", "PATCH":
		var body struct {
			Active     *ActiveView `json:"active"`
			Provider   *string     `json:"activeProvider"`
			Lang       *string     `json:"lang"`
			BypassMode *bool       `json:"bypassMode"`
			LogLevel   *string     `json:"logLevel"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.updateConfig(func(cfg *provider.Config) error {
			normal := body.Provider
			if body.Active != nil {
				normal = &body.Active.Normal
			}
			if normal != nil {
				name := strings.TrimSpace(*normal)
				if err := validatorForConfig(cfg, name); err != nil {
					return err
				}
				if provider.IsDefaultProvider(name) {
					cfg.LegacyClaudeDefault = ""
				}
				cfg.ActiveProvider = name
			}
			if body.Lang != nil {
				cfg.Lang = strings.TrimSpace(*body.Lang)
			}
			if body.BypassMode != nil {
				cfg.BypassMode = *body.BypassMode
			}
			if body.LogLevel != nil {
				level := strings.ToLower(strings.TrimSpace(*body.LogLevel))
				switch level {
				case "", "off", "debug", "info", "warn", "error":
					cfg.LogLevel = level
				default:
					return fmt.Errorf("logLevel: expected off, debug, info, warn or error, got %q", *body.LogLevel)
				}
			}
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleConfigProviders serves POST /api/config/providers (create or overwrite).
func (s *Server) handleConfigProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "PUT" {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		ProviderInput
		Name string `json:"name"`
	}
	if err := decodeBody(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validateProviderName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var created bool
	if err := s.updateConfig(func(cfg *provider.Config) error {
		existing, exists := cfg.Providers[name]
		if input.OAuthProvider != nil && *input.OAuthProvider == "" && input.Type == nil {
			return fmt.Errorf("type is required: an OAuth provider needs its runtime type")
		}
		if !exists && (input.Type == nil || strings.TrimSpace(*input.Type) == "") && (input.OAuthProvider == nil || strings.TrimSpace(*input.OAuthProvider) == "") {
			return fmt.Errorf("type is required for a new provider")
		}
		p := existing
		if !exists {
			p = provider.Provider{Name: name}
		}
		p.Name = name
		if err := applyProviderInput(&p, &input.ProviderInput, true); err != nil {
			return err
		}
		if strings.TrimSpace(p.Endpoint) == "" && strings.TrimSpace(p.OAuthProvider) == "" {
			return fmt.Errorf("endpoint is required")
		}
		if err := provider.ValidateRuntime(p); err != nil {
			return err
		}
		cfg.Providers[name] = p
		created = !exists
		return nil
	}); err != nil {
		writeConfigError(w, err)
		return
	}
	s.invalidateProviderCatalog(name)
	view, err := s.configView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, view)
}

// handleConfigProvider serves /api/config/providers/{name}[/test].
func (s *Server) handleConfigProvider(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/config/providers/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if provider.IsDefaultProvider(name) {
		writeError(w, http.StatusForbidden, "the default selector is reserved and cannot be edited")
		return
	}
	if action == "test" {
		if r.Method != "POST" {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleConfigProviderTest(w, r, name)
		return
	}
	if action != "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case "GET":
		cfg, err := config.Load()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, ok := cfg.Providers[name]
		if !ok {
			writeError(w, http.StatusNotFound, "provider not found")
			return
		}
		writeJSON(w, http.StatusOK, s.providerView(cfg, name, p))

	case "PATCH", "PUT":
		var input ProviderInput
		raw, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			writeError(w, http.StatusBadRequest, "decode request body: "+err.Error())
			return
		}
		clearKey := false
		clearDefaultModel := false
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			if value, ok := fields["apiKey"]; ok {
				if string(value) == "null" {
					clearKey = true
					input.APIKey = nil
				} else if input.APIKey != nil && strings.TrimSpace(*input.APIKey) == "" {
					input.APIKey = nil
				}
			}
			if value, ok := fields["defaultModel"]; ok && string(value) == "null" {
				clearDefaultModel = true
				input.DefaultModel = nil
			}
		}
		if err := s.updateConfig(func(cfg *provider.Config) error {
			p, ok := cfg.Providers[name]
			if !ok {
				return errNoProvider
			}
			p.Name = name
			if clearKey {
				p.APIKey = ""
			}
			if clearDefaultModel {
				p.DefaultModel = ""
			}
			if err := applyProviderInput(&p, &input, false); err != nil {
				return err
			}
			if err := provider.ValidateRuntime(p); err != nil {
				return err
			}
			cfg.Providers[name] = p
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		s.invalidateProviderCatalog(name)
		view, err := s.configView()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)

	case "DELETE":
		if err := s.updateConfig(func(cfg *provider.Config) error {
			if _, ok := cfg.Providers[name]; !ok {
				return errNoProvider
			}
			if strings.TrimSpace(cfg.ActiveProvider) == name {
				return fmt.Errorf("provider %q is the active provider; switch first", name)
			}
			delete(cfg.Providers, name)
			return nil
		}); err != nil {
			writeConfigError(w, err)
			return
		}
		s.invalidateProviderCatalog(name)
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleConfigProviderTest probes a provider's endpoint or runtime once. Nothing is saved.
func (s *Server) handleConfigProviderTest(w http.ResponseWriter, r *http.Request, name string) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := cfg.Providers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	p.Name = name
	var body struct {
		Endpoint *string `json:"endpoint"`
		APIKey   *string `json:"apiKey"`
		Type     *string `json:"type"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	customProbe := false
	if body.Endpoint != nil && strings.TrimSpace(*body.Endpoint) != "" {
		p.Endpoint = strings.TrimSpace(*body.Endpoint)
		customProbe = true
	}
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		p.APIKey = strings.TrimSpace(*body.APIKey)
		customProbe = true
	}
	if body.Type != nil && strings.TrimSpace(*body.Type) != "" {
		p.Type = strings.TrimSpace(*body.Type)
		customProbe = true
	}
	result, catalog := probeProviderCatalog(r.Context(), p)
	if result.OK && !customProbe {
		s.recordProviderCatalog(name, catalog.models, catalog.labels, catalog.details)
	}
	if result.OK && len(result.Labels) > 0 {
		if err := s.updateConfig(func(cfg *provider.Config) error {
			stored, ok := cfg.Providers[name]
			if !ok {
				return nil
			}
			mergeModelLabels(&stored, result.Labels)
			cfg.Providers[name] = stored
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// ProbeResult reports one connectivity check.
type ProbeResult struct {
	OK      bool              `json:"ok"`
	Status  int               `json:"status,omitempty"`
	Models  []string          `json:"models,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Message string            `json:"message,omitempty"`
	Elapsed int64             `json:"elapsedMs"`
}

// maxProbeModels bounds the model ids one probe returns.
const maxProbeModels = 2000

const providerCatalogTTL = 5 * time.Minute

type providerRuntimeCatalog struct {
	models    []string
	labels    map[string]string
	details   map[string]protocol.ModelInfo
	fetchedAt time.Time
}

type catalogRefreshCall struct {
	done    chan struct{}
	catalog providerRuntimeCatalog
	err     error
}

func (s *Server) recordProviderCatalog(name string, models []string, labels map[string]string, details map[string]protocol.ModelInfo) {
	if s == nil || strings.TrimSpace(name) == "" || len(models) == 0 {
		return
	}
	copied := providerRuntimeCatalog{
		models:    append([]string(nil), models...),
		labels:    make(map[string]string, len(labels)),
		details:   make(map[string]protocol.ModelInfo, len(details)),
		fetchedAt: time.Now(),
	}
	for k, v := range labels {
		copied.labels[k] = v
	}
	for k, v := range details {
		copied.details[strings.ToLower(strings.TrimSpace(k))] = v
	}
	s.catalogMu.Lock()
	if s.catalogs == nil {
		s.catalogs = make(map[string]providerRuntimeCatalog)
	}
	s.catalogs[name] = copied
	s.catalogMu.Unlock()
}

func (s *Server) invalidateProviderCatalog(name string) {
	if s == nil || strings.TrimSpace(name) == "" {
		return
	}
	s.catalogMu.Lock()
	delete(s.catalogs, name)
	s.catalogMu.Unlock()
}

func (s *Server) cachedProviderCatalog(name string) (providerRuntimeCatalog, bool) {
	if s == nil || strings.TrimSpace(name) == "" {
		return providerRuntimeCatalog{}, false
	}
	s.catalogMu.Lock()
	cat, ok := s.catalogs[name]
	s.catalogMu.Unlock()
	if !ok || time.Since(cat.fetchedAt) > providerCatalogTTL {
		return providerRuntimeCatalog{}, false
	}
	return cat, true
}

func (s *Server) refreshProviderCatalog(ctx context.Context, name string, p provider.Provider) (providerRuntimeCatalog, error) {
	if s == nil {
		res, cat := probeProviderCatalog(ctx, p)
		if !res.OK {
			return providerRuntimeCatalog{}, fmt.Errorf("%s", res.Message)
		}
		return cat, nil
	}
	s.catalogMu.Lock()
	if s.catalogInflight == nil {
		s.catalogInflight = make(map[string]*catalogRefreshCall)
	}
	if call, exists := s.catalogInflight[name]; exists {
		s.catalogMu.Unlock()
		select {
		case <-call.done:
			return call.catalog, call.err
		case <-ctx.Done():
			return providerRuntimeCatalog{}, ctx.Err()
		}
	}
	call := &catalogRefreshCall{done: make(chan struct{})}
	s.catalogInflight[name] = call
	s.catalogMu.Unlock()

	res, cat := probeProviderCatalog(ctx, p)
	if !res.OK {
		call.err = fmt.Errorf("%s", res.Message)
	} else {
		call.catalog = cat
		s.recordProviderCatalog(name, cat.models, cat.labels, cat.details)
		if len(cat.labels) > 0 {
			_ = s.updateConfig(func(cfg *provider.Config) error {
				stored, ok := cfg.Providers[name]
				if !ok {
					return nil
				}
				mergeModelLabels(&stored, cat.labels)
				cfg.Providers[name] = stored
				return nil
			})
		}
	}
	close(call.done)

	s.catalogMu.Lock()
	delete(s.catalogInflight, name)
	s.catalogMu.Unlock()
	return call.catalog, call.err
}

// handleProviderSubroute dispatches /api/providers/{name}/models.
func (s *Server) handleProviderSubroute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "models" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.handleProviderModels(w, r, parts[0])
}

func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request, name string) {
	if provider.IsDefaultProvider(name) {
		writeJSON(w, http.StatusOK, map[string]any{
			"models":       []ModelChoice{},
			"effortLevels": oauthproxy.DefaultNativeGrokEffortLevels(),
			"refreshed":    false,
		})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := cfg.Providers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	p.Name = name
	forceRefresh := r.URL.Query().Get("refresh") == "1"
	refreshed := false
	if !forceRefresh {
		if _, cached := s.cachedProviderCatalog(name); cached {
			writeJSON(w, http.StatusOK, map[string]any{
				"models":    s.modelChoicesForProvider(name, p),
				"refreshed": false,
			})
			return
		}
	}
	if _, err := s.refreshProviderCatalog(r.Context(), name, p); err == nil {
		refreshed = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":    s.modelChoicesForProvider(name, p),
		"refreshed": refreshed,
	})
}

// probeProvider checks runtime validity and lists available models: via the
// embedded runtime for OAuth subscriptions, or via /models for gateways.
func probeProvider(ctx context.Context, p provider.Provider) ProbeResult {
	res, _ := probeProviderCatalog(ctx, p)
	return res
}

func probeProviderCatalog(ctx context.Context, p provider.Provider) (ProbeResult, providerRuntimeCatalog) {
	start := time.Now()
	if p.Name == "" {
		p.Name = "probe"
	}
	if err := provider.ValidateRuntime(p); err != nil {
		return ProbeResult{OK: false, Message: redactSecrets(err.Error()), Elapsed: time.Since(start).Milliseconds()}, providerRuntimeCatalog{}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	type outcome struct {
		models  []string
		labels  map[string]string
		details map[string]protocol.ModelInfo
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		if strings.TrimSpace(p.OAuthProvider) != "" {
			session, err := providersession.Prepare(ctx, p)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			models := session.Runtime.Models()
			labels := make(map[string]string, len(models))
			rawDetails := session.Runtime.ModelDetails()
			details := make(map[string]protocol.ModelInfo, len(rawDetails))
			for id, info := range rawDetails {
				details[strings.ToLower(strings.TrimSpace(id))] = info
				if name := strings.TrimSpace(info.DisplayName); name != "" && name != id {
					labels[id] = name
				}
			}
			for id, name := range session.Runtime.ModelDisplayNames() {
				if name = strings.TrimSpace(name); name != "" && name != id {
					labels[id] = name
				}
			}
			session.Close()
			done <- outcome{models: models, labels: labels, details: details}
			return
		}
		infos, err := protocol.GetOpenAIModelInfos(p.Endpoint, p.APIKey)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		models := make([]string, 0, len(infos))
		labels := make(map[string]string, len(infos))
		details := make(map[string]protocol.ModelInfo, len(infos))
		for _, info := range infos {
			models = append(models, info.ID)
			details[strings.ToLower(strings.TrimSpace(info.ID))] = info
			if name := strings.TrimSpace(info.DisplayName); name != "" && name != info.ID {
				labels[info.ID] = name
			}
		}
		done <- outcome{models: models, labels: labels, details: details}
	}()
	select {
	case <-ctx.Done():
		return ProbeResult{OK: false, Message: "timed out", Elapsed: time.Since(start).Milliseconds()}, providerRuntimeCatalog{}
	case result := <-done:
		if result.err != nil {
			return ProbeResult{OK: false, Message: redactSecrets(result.err.Error()), Elapsed: time.Since(start).Milliseconds()}, providerRuntimeCatalog{}
		}
		models := result.models
		if len(models) > maxProbeModels {
			models = models[:maxProbeModels]
		}
		catalog := providerRuntimeCatalog{
			models:    append([]string(nil), models...),
			labels:    result.labels,
			details:   result.details,
			fetchedAt: time.Now(),
		}
		return ProbeResult{OK: true, Status: 200, Models: models, Labels: result.labels, Elapsed: time.Since(start).Milliseconds()}, catalog
	}
}

// mergeModelLabels records display names a probe or session observed, keyed by
// model ID. Existing labels win so a name the user's catalog stopped
// reporting does not overwrite fresher information.
func mergeModelLabels(p *provider.Provider, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	if p.ModelLabels == nil {
		p.ModelLabels = make(map[string]string, len(labels))
	}
	for id, name := range labels {
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || name == "" || name == id {
			continue
		}
		if _, exists := p.ModelLabels[id]; !exists {
			p.ModelLabels[id] = name
		}
	}
}

func (s *Server) updateConfig(fn func(*provider.Config) error) error {
	return config.Update(fn)
}

func (s *Server) configView() (ConfigView, error) {
	cfg, err := config.Load()
	if err != nil {
		return ConfigView{}, err
	}
	view := ConfigView{
		Active: ActiveView{Normal: cfg.ActiveProvider},
		Lang:   cfg.Lang,
		Bypass: cfg.BypassMode,
		Default: DefaultView{
			Name:         provider.DefaultProvider,
			IsDefault:    true,
			Active:       provider.IsDefaultProvider(cfg.ActiveProvider),
			EffortLevels: oauthproxy.DefaultNativeGrokEffortLevels(),
			LegacyClaude: cfg.LegacyClaudeDefault,
		},
		LogLevel:  cfg.LogLevel,
		Providers: make(map[string]ProviderView, len(cfg.Providers)),
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		view.Providers[name] = s.providerView(cfg, name, cfg.Providers[name])
		view.Order = append(view.Order, name)
	}
	if view.Order == nil {
		view.Order = []string{}
	}
	return view, nil
}

// ModelChoice is one entry of a conversation's model picker.
type ModelChoice struct {
	Value          string   `json:"value"`
	Label          string   `json:"label"`
	SupportsEffort bool     `json:"supportsEffort"`
	EffortLevels   []string `json:"effortLevels,omitempty"`
	DefaultEffort  string   `json:"defaultEffort,omitempty"`
}

func (s *Server) modelChoicesForProvider(name string, p provider.Provider) []ModelChoice {
	var cachedPtr *providerRuntimeCatalog
	if cat, ok := s.cachedProviderCatalog(name); ok {
		cachedPtr = &cat
	}
	return modelChoicesWithCatalog(p, cachedPtr)
}

// modelChoices lists the models a turn can ask for from p's model pool.
func modelChoices(p provider.Provider) []ModelChoice {
	return modelChoicesWithCatalog(p, nil)
}

func modelChoicesWithCatalog(p provider.Provider, cached *providerRuntimeCatalog) []ModelChoice {
	staticModels, mergedLabels, mergedDetails, mdProviderID := oauthproxy.StaticProviderModelDetails(p)
	modelList := staticModels
	if cached != nil && len(cached.models) > 0 {
		merged := make([]string, 0, len(cached.models)+len(staticModels))
		seenRaw := make(map[string]bool, len(cached.models)+len(staticModels))
		for _, id := range append(append([]string(nil), cached.models...), staticModels...) {
			trimmed := strings.TrimSpace(id)
			key := strings.ToLower(trimmed)
			if key == "" || seenRaw[key] {
				continue
			}
			seenRaw[key] = true
			merged = append(merged, trimmed)
		}
		if def := provider.EffectiveDefaultModel(p); def != "" {
			defBase := strings.ToLower(stripOneM(def))
			for i, m := range merged {
				if strings.EqualFold(stripOneM(m), defBase) {
					if i > 0 {
						merged = append([]string{m}, append(merged[:i], merged[i+1:]...)...)
					}
					break
				}
			}
		}
		modelList = merged
		for id, name := range cached.labels {
			id, name = strings.TrimSpace(id), strings.TrimSpace(name)
			if id != "" && name != "" {
				mergedLabels[id] = name
			}
		}
		for id, info := range cached.details {
			mergedDetails[strings.ToLower(strings.TrimSpace(id))] = info
		}
	}

	choices := []ModelChoice{}
	seen := map[string]bool{}
	index, _ := modelsdev.CachedIndex()
	explicitEffort := groklaunch.NormalizeReasoningEffort(p.EffortLevel)

	for _, raw := range modelList {
		value := stripOneM(raw)
		if value == "" || seen[strings.ToLower(value)] {
			continue
		}
		seen[strings.ToLower(value)] = true

		detail, hasDetail := mergedDetails[strings.ToLower(raw)]
		if !hasDetail {
			detail, hasDetail = mergedDetails[strings.ToLower(value)]
		}
		rawLabel := lookupCaseInsensitiveLabel(mergedLabels, value)
		if rawLabel == "" {
			rawLabel = lookupCaseInsensitiveLabel(mergedLabels, raw)
		}
		if rawLabel == "" {
			rawLabel = strings.TrimSpace(detail.DisplayName)
		}

		choice := ModelChoice{Value: value, Label: value}
		if rawLabel != "" && !strings.EqualFold(rawLabel, value) {
			choice.Label = rawLabel + " (" + value + ")"
		}

		supports, levels, resolvedDefault := oauthproxy.ResolveGrokModelEffort(index, mdProviderID, value, rawLabel, detail, hasDetail)
		choice.SupportsEffort = supports
		if supports && len(levels) > 0 {
			choice.EffortLevels = levels
			choice.DefaultEffort = resolvedDefault
			if explicitEffort != "" && explicitEffort != "none" {
				choice.DefaultEffort = closestEffortLevel(levels, explicitEffort)
			}
		}
		choices = append(choices, choice)
	}
	return choices
}

func lookupCaseInsensitiveLabel(labels map[string]string, id string) string {
	if len(labels) == 0 || strings.TrimSpace(id) == "" {
		return ""
	}
	if v := strings.TrimSpace(labels[id]); v != "" {
		return v
	}
	for k, v := range labels {
		if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(id)) {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func closestEffortLevel(levels []string, target string) string {
	for _, lvl := range levels {
		if strings.EqualFold(lvl, target) {
			return lvl
		}
	}
	return pickManagedEffort(levels, strings.ToLower(strings.TrimSpace(target)))
}

func (s *Server) providerView(cfg *provider.Config, name string, p provider.Provider) ProviderView {
	return providerViewWithChoices(cfg, name, p, s.modelChoicesForProvider(name, p))
}

func providerView(cfg *provider.Config, name string, p provider.Provider) ProviderView {
	return providerViewWithChoices(cfg, name, p, modelChoices(p))
}

func providerViewWithChoices(cfg *provider.Config, name string, p provider.Provider, choices []ModelChoice) ProviderView {
	return ProviderView{
		Name:          name,
		Type:          provider.EffectiveType(p),
		Endpoint:      p.Endpoint,
		APIKey:        maskKey(p.APIKey),
		HasKey:        strings.TrimSpace(p.APIKey) != "",
		OAuthProvider: p.OAuthProvider,
		EffortLevel:   p.EffortLevel,
		Models:        p.Model,
		DefaultModel:  p.DefaultModel,
		FastMode:      p.FastMode,
		ContextPreset: p.ContextPreset,
		ContextWindow: presetContextWindow(p),
		Active:        cfg.ActiveProvider == name,
		ModelChoices:  choices,
	}
}

// presetContextWindow is the context size p's Balanced preset declares, or
// zero for Default.
func presetContextWindow(p provider.Provider) int {
	maxContext, _, _, ok := provider.ContextPresetValues(provider.ProviderContextPreset(p))
	if !ok {
		return 0
	}
	window, _ := strconv.Atoi(maxContext)
	return window
}

// maskKey keeps the last four characters; everything else is hidden.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= apiMaskSuffix {
		return strings.Repeat("*", len(runes))
	}
	return "***" + string(runes[len(runes)-apiMaskSuffix:])
}

// applyProviderInput copies the fields present in input onto p. clearKey lets an
// empty apiKey nil out the credential; otherwise an empty string leaves it.
func applyProviderInput(p *provider.Provider, input *ProviderInput, clearKey bool) error {
	if input.Type != nil {
		p.Type = strings.TrimSpace(*input.Type)
	}
	if input.Endpoint != nil {
		p.Endpoint = strings.TrimSpace(*input.Endpoint)
	}
	if input.APIKey != nil {
		key := strings.TrimSpace(*input.APIKey)
		switch {
		case key == "" && clearKey:
			p.APIKey = ""
		case key != "":
			p.APIKey = key
		}
	} else if clearKey {
		p.APIKey = ""
	}
	if input.OAuthProvider != nil {
		p.OAuthProvider = strings.ToLower(strings.TrimSpace(*input.OAuthProvider))
	}
	if input.Models != nil {
		p.Model = strings.TrimSpace(*input.Models)
	}
	if input.DefaultModel != nil {
		p.DefaultModel = provider.NormalizeModelID(*input.DefaultModel)
	}
	if input.EffortLevel != nil {
		level := strings.ToLower(strings.TrimSpace(*input.EffortLevel))
		if level == "default" {
			level = ""
		}
		if level != "" && !containsString(EffortLevels, level) {
			return fmt.Errorf("effortLevel: expected one of %s, got %q", strings.Join(EffortLevels, ", "), *input.EffortLevel)
		}
		p.EffortLevel = level
	}
	if input.FastMode != nil {
		p.FastMode = *input.FastMode
	}
	if input.ContextPreset != nil {
		p.ContextPreset = strings.TrimSpace(*input.ContextPreset)
	}
	return nil
}

func validateProviderName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if provider.IsDefaultProvider(name) {
		return fmt.Errorf("the name %q is reserved for Grok Build's own configuration", name)
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
		default:
			return fmt.Errorf("name may only contain letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

func validatorForConfig(cfg *provider.Config, name string) error {
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if provider.IsDefaultProvider(name) {
		return nil
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return fmt.Errorf("provider %q not found; use ccl ls or ccl set", name)
	}
	p.Name = name
	return provider.ValidateRuntime(p)
}

func writeConfigError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case err == errNoProvider:
		writeError(w, http.StatusNotFound, "provider not found")
	case errorsIsConflict(err):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func errorsIsConflict(err error) bool {
	message := err.Error()
	return strings.Contains(message, "is the active provider")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return data, nil
}
