package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ugorji/go/codec"
	htmlparser "golang.org/x/net/html"
)

const (
	kiroModelsCacheTTL   = time.Hour
	kiroModelsAPIVersion = "0.9.2"
)

var kiroAvailableModelsEndpoint = func(region string) string {
	return "https://q." + region + ".amazonaws.com/ListAvailableModels?origin=AI_EDITOR"
}

var (
	kiroWebPortalHomeEndpoint   = "https://app.kiro.dev/home"
	kiroWebPortalModelsEndpoint = "https://app.kiro.dev/service/KiroWebPortalService/operation/ListAvailableModels"
	kiroCBORHandle              = newKiroCBORHandle()
)

func newKiroCBORHandle() *codec.CborHandle {
	handle := &codec.CborHandle{IndefiniteLength: true, SkipUnexpectedTags: true}
	handle.MaxDepth = 32
	handle.MaxInitLen = 4096
	return handle
}

type kiroTokenLimits struct {
	MaxInputTokens  int64 `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
}

type kiroAvailableModel struct {
	ModelID             string           `json:"modelId" codec:"modelId"`
	ModelName           string           `json:"modelName,omitempty" codec:"modelName,omitempty"`
	Description         string           `json:"description,omitempty" codec:"description,omitempty"`
	TokenLimits         *kiroTokenLimits `json:"tokenLimits,omitempty" codec:"tokenLimits,omitempty"`
	RateMultiplier      *float64         `json:"rateMultiplier,omitempty" codec:"rateMultiplier,omitempty"`
	RateUnit            string           `json:"rateUnit,omitempty" codec:"rateUnit,omitempty"`
	SupportedInputTypes []string         `json:"supportedInputTypes,omitempty" codec:"supportedInputTypes,omitempty"`
}

type kiroAvailableModelsResponse struct {
	Models []kiroAvailableModel `json:"models"`
}

type kiroPortalModelsResponse struct {
	Models       []kiroAvailableModel `codec:"models"`
	DefaultModel *kiroAvailableModel  `codec:"defaultModel,omitempty"`
	NextToken    string               `codec:"nextToken,omitempty"`
}

type kiroPortalModelsRequest struct {
	CSRFToken  string `codec:"csrfToken"`
	ProfileARN string `codec:"profileArn"`
}

type kiroModelCacheEntry struct {
	models    []kiroAvailableModel
	fetchedAt time.Time
}

// kiroModelFetch is one in-flight discovery call. Concurrent requests for the
// same credential wait on it instead of issuing their own upstream call.
type kiroModelFetch struct {
	done   chan struct{}
	models []kiroAvailableModel
	err    error
}

type kiroModelCatalog struct {
	// mu guards cache and inflight only. It is never held across network I/O.
	mu                   sync.Mutex
	cache                map[string]kiroModelCacheEntry
	inflight             map[string]*kiroModelFetch
	ttl                  time.Duration
	endpoint             func(string) string
	portalHomeEndpoint   string
	portalModelsEndpoint string
}

func newKiroModelCatalog(endpoint func(string) string) *kiroModelCatalog {
	if endpoint == nil {
		endpoint = kiroAvailableModelsEndpoint
	}
	return &kiroModelCatalog{
		cache:                make(map[string]kiroModelCacheEntry),
		inflight:             make(map[string]*kiroModelFetch),
		ttl:                  kiroModelsCacheTTL,
		endpoint:             endpoint,
		portalHomeEndpoint:   kiroWebPortalHomeEndpoint,
		portalModelsEndpoint: kiroWebPortalModelsEndpoint,
	}
}

func (c *kiroModelCatalog) cached(path string) (kiroModelCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[path]
	return entry, ok
}

func (c *kiroModelCatalog) store(path string, models []kiroAvailableModel, fetchedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]kiroModelCacheEntry)
	}
	c.cache[path] = kiroModelCacheEntry{models: models, fetchedAt: fetchedAt}
}

// retain evicts cached catalogs for credentials that are no longer selected, so
// removed accounts do not linger for the lifetime of the process.
func (c *kiroModelCatalog) retain(live map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.cache {
		if _, ok := live[path]; !ok {
			delete(c.cache, path)
		}
	}
}

// fetchOnce discovers the catalog of one credential, collapsing concurrent
// callers for that credential into a single upstream request.
func (c *kiroModelCatalog) fetchOnce(ctx context.Context, service *kiroService, candidate *kiroCredential) ([]kiroAvailableModel, error) {
	c.mu.Lock()
	if c.inflight == nil {
		c.inflight = make(map[string]*kiroModelFetch)
	}
	if existing, ok := c.inflight[candidate.path]; ok {
		c.mu.Unlock()
		select {
		case <-existing.done:
			return existing.models, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fetch := &kiroModelFetch{done: make(chan struct{})}
	c.inflight[candidate.path] = fetch
	c.mu.Unlock()

	fetch.models, fetch.err = c.fetchCredentialModels(ctx, service, candidate)
	close(fetch.done)

	c.mu.Lock()
	delete(c.inflight, candidate.path)
	c.mu.Unlock()
	return fetch.models, fetch.err
}

// availableModels discovers the actual catalog for every credential selected by
// this runtime. A fresh result is cached per credential; if a later refresh
// fails, its last successful result remains usable.
func (s *kiroService) availableModels(ctx context.Context) ([]kiroAvailableModel, error) {
	catalog := s.modelCatalog
	if catalog == nil {
		catalog = newKiroModelCatalog(nil)
	}

	credentials, err := s.pool.load()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no usable Kiro credentials")
	}

	now := time.Now()
	merged := make(map[string]kiroAvailableModel)
	var discoveryErrors []error
	successes := 0
	type pendingCredential struct {
		credential *kiroCredential
		cached     kiroModelCacheEntry
		hasCache   bool
	}
	pending := make([]pendingCredential, 0, len(credentials))
	live := make(map[string]struct{}, len(credentials))
	for _, candidate := range credentials {
		live[candidate.path] = struct{}{}
		cached, hasCache := catalog.cached(candidate.path)
		if hasCache && now.Sub(cached.fetchedAt) < catalog.ttl {
			mergeKiroAvailableModels(merged, cached.models)
			successes++
			continue
		}
		pending = append(pending, pendingCredential{
			credential: candidate,
			cached:     cached,
			hasCache:   hasCache,
		})
	}
	catalog.retain(live)

	type discoveryResult struct {
		pending pendingCredential
		models  []kiroAvailableModel
		err     error
	}
	results := make(chan discoveryResult, len(pending))
	limit := make(chan struct{}, 4)
	for _, item := range pending {
		item := item
		go func() {
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				results <- discoveryResult{pending: item, err: ctx.Err()}
				return
			}
			models, fetchErr := catalog.fetchOnce(ctx, s, item.credential)
			results <- discoveryResult{pending: item, models: models, err: fetchErr}
		}()
	}
	for range pending {
		result := <-results
		if result.err != nil {
			if result.pending.hasCache {
				mergeKiroAvailableModels(merged, result.pending.cached.models)
				successes++
				LogWarnf("kiro model discovery uses stale cache credential=%q error=%v", result.pending.credential.fileName, result.err)
				continue
			}
			discoveryErrors = append(discoveryErrors, fmt.Errorf("%s: %w", result.pending.credential.fileName, result.err))
			continue
		}

		copied := cloneKiroAvailableModels(result.models)
		catalog.store(result.pending.credential.path, copied, now)
		mergeKiroAvailableModels(merged, copied)
		successes++
	}

	if successes == 0 {
		if len(discoveryErrors) == 0 {
			return nil, fmt.Errorf("Kiro model discovery returned no credential results")
		}
		return nil, fmt.Errorf("Kiro model discovery failed: %w", errors.Join(discoveryErrors...))
	}

	// Preserve explicitly configured ccl aliases (notably [1m]) only when their
	// normalized backend model is present in the account's upstream catalog.
	for _, alias := range s.models {
		backendID := mapKiroModel(alias)
		upstream, ok := merged[strings.ToLower(backendID)]
		if !ok {
			continue
		}
		if _, exists := merged[strings.ToLower(alias)]; exists {
			continue
		}
		aliased := upstream
		aliased.ModelID = alias
		merged[strings.ToLower(alias)] = aliased
	}

	models := make([]kiroAvailableModel, 0, len(merged))
	for _, model := range merged {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].ModelID) < strings.ToLower(models[j].ModelID)
	})
	return models, nil
}

// cloneKiroAvailableModel deep-copies the pointer and slice fields so cached and
// merged entries never share mutable state with the decoded response.
func cloneKiroAvailableModel(model kiroAvailableModel) kiroAvailableModel {
	cloned := model
	if model.TokenLimits != nil {
		limits := *model.TokenLimits
		cloned.TokenLimits = &limits
	}
	if model.RateMultiplier != nil {
		multiplier := *model.RateMultiplier
		cloned.RateMultiplier = &multiplier
	}
	cloned.SupportedInputTypes = append([]string(nil), model.SupportedInputTypes...)
	return cloned
}

func cloneKiroAvailableModels(models []kiroAvailableModel) []kiroAvailableModel {
	cloned := make([]kiroAvailableModel, len(models))
	for index, model := range models {
		cloned[index] = cloneKiroAvailableModel(model)
	}
	return cloned
}

func mergeKiroAvailableModels(merged map[string]kiroAvailableModel, incoming []kiroAvailableModel) {
	for _, incomingModel := range incoming {
		model := cloneKiroAvailableModel(incomingModel)
		model.ModelID = strings.TrimSpace(model.ModelID)
		if model.ModelID == "" {
			continue
		}
		key := strings.ToLower(model.ModelID)
		current, exists := merged[key]
		if !exists {
			merged[key] = model
			continue
		}
		if current.ModelName == "" {
			current.ModelName = model.ModelName
		}
		if current.Description == "" {
			current.Description = model.Description
		}
		if current.RateMultiplier == nil {
			current.RateMultiplier = model.RateMultiplier
		}
		if current.RateUnit == "" {
			current.RateUnit = model.RateUnit
		}
		if len(current.SupportedInputTypes) == 0 {
			current.SupportedInputTypes = append([]string(nil), model.SupportedInputTypes...)
		}
		if current.TokenLimits == nil {
			current.TokenLimits = model.TokenLimits
		} else if model.TokenLimits != nil {
			if current.TokenLimits.MaxInputTokens == 0 {
				current.TokenLimits.MaxInputTokens = model.TokenLimits.MaxInputTokens
			}
			if current.TokenLimits.MaxOutputTokens == 0 {
				current.TokenLimits.MaxOutputTokens = model.TokenLimits.MaxOutputTokens
			}
		}
		merged[key] = current
	}
}

func (c *kiroModelCatalog) fetchCredentialModels(ctx context.Context, service *kiroService, candidate *kiroCredential) ([]kiroAvailableModel, error) {
	credential, err := service.pool.usableCredential(ctx, candidate, false)
	if err != nil {
		return nil, err
	}
	models, err := c.fetchCredentialModelsWithToken(ctx, service, credential)
	var upstreamErr *kiroUpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.status != http.StatusUnauthorized {
		return models, err
	}

	refreshed, refreshErr := service.pool.usableCredential(ctx, credential, true)
	if refreshErr != nil {
		return nil, refreshErr
	}
	return c.fetchCredentialModelsWithToken(ctx, service, refreshed)
}

func (c *kiroModelCatalog) fetchCredentialModelsWithToken(ctx context.Context, service *kiroService, credential *kiroCredential) ([]kiroAvailableModel, error) {
	if models, attempted, err := c.fetchPortalModels(ctx, service, credential); attempted {
		if err == nil {
			LogInfof("kiro web portal model discovery credential=%q model_count=%d", credential.fileName, len(models))
			return models, nil
		}
		LogWarnf("kiro web portal model discovery fallback credential=%q error=%v", credential.fileName, err)
	}

	regions := kiroRESTRegionCandidates(credential)
	var lastErr error
	for index, region := range regions {
		models, err := c.requestAvailableModels(ctx, service, credential, region)
		if err == nil {
			LogInfof("kiro model discovery credential=%q region=%q model_count=%d", credential.fileName, region, len(models))
			return models, nil
		}
		lastErr = err
		var upstreamErr *kiroUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.status == http.StatusForbidden && index+1 < len(regions) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (c *kiroModelCatalog) fetchPortalModels(ctx context.Context, service *kiroService, credential *kiroCredential) ([]kiroAvailableModel, bool, error) {
	session := *credential
	if session.csrfToken == "" {
		var attempted bool
		var err error
		session, attempted, err = c.bootstrapPortalSession(ctx, service, session)
		if !attempted || err != nil {
			return nil, attempted, err
		}
	}

	models, err := c.requestPortalModels(ctx, service, &session)
	if err == nil {
		return models, true, nil
	}

	// A CSRF token captured from an older web session can expire independently
	// from the desktop access token. Refresh the portal metadata once before
	// falling back to the Amazon Q catalog.
	refreshed, attempted, bootstrapErr := c.bootstrapPortalSession(ctx, service, session)
	if !attempted || bootstrapErr != nil || refreshed.csrfToken == session.csrfToken {
		return nil, true, err
	}
	models, retryErr := c.requestPortalModels(ctx, service, &refreshed)
	if retryErr != nil {
		return nil, true, retryErr
	}
	return models, true, nil
}

func (c *kiroModelCatalog) bootstrapPortalSession(ctx context.Context, service *kiroService, credential kiroCredential) (kiroCredential, bool, error) {
	idp := kiroPortalIDP(&credential)
	if idp == "" || credential.accessToken == "" || credential.refreshToken == "" {
		return credential, false, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.portalHomeEndpoint, nil)
	if err != nil {
		return credential, true, err
	}
	addKiroPortalCookies(request, &credential, idp)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("User-Agent", "ccl KiroWebPortal model discovery")
	response, err := service.client.Do(request)
	if err != nil {
		return credential, true, fmt.Errorf("load Kiro Web Portal session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return credential, true, fmt.Errorf("load Kiro Web Portal session: HTTP %d", response.StatusCode)
	}
	metadata, err := parseKiroPortalMetadata(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return credential, true, fmt.Errorf("parse Kiro Web Portal session: %w", err)
	}
	credential.csrfToken = metadata["csrf-token"]
	if credential.csrfToken == "" {
		return credential, true, fmt.Errorf("Kiro Web Portal session did not return a CSRF token")
	}
	if value := metadata["user-id"]; value != "" {
		credential.userID = value
	}
	if value := metadata["profile-arn"]; value != "" {
		credential.profileARN = value
	}
	if value := metadata["idp"]; value != "" {
		credential.provider = value
	}
	return credential, true, nil
}

func (c *kiroModelCatalog) requestPortalModels(ctx context.Context, service *kiroService, credential *kiroCredential) ([]kiroAvailableModel, error) {
	session := *credential
	session.visitorID = kiroPortalVisitorID(credential)
	credential = &session
	profileARN := credential.streamingProfileARN()
	if credential.csrfToken == "" || profileARN == "" {
		return nil, fmt.Errorf("Kiro Web Portal credential is missing CSRF token or profile ARN")
	}
	var body []byte
	if err := codec.NewEncoderBytes(&body, kiroCBORHandle).Encode(kiroPortalModelsRequest{
		CSRFToken:  credential.csrfToken,
		ProfileARN: profileARN,
	}); err != nil {
		return nil, fmt.Errorf("encode Kiro Web Portal model request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.portalModelsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/cbor")
	request.Header.Set("Content-Type", "application/cbor")
	request.Header.Set("smithy-protocol", "rpc-v2-cbor")
	request.Header.Set("Authorization", "Bearer "+credential.accessToken)
	request.Header.Set("X-CSRF-Token", credential.csrfToken)
	request.Header.Set("Origin", "https://app.kiro.dev")
	request.Header.Set("Referer", "https://app.kiro.dev/home")
	request.Header.Set("amz-sdk-invocation-id", uuidString())
	request.Header.Set("amz-sdk-request", "attempt=1; max=1")
	request.Header.Set("x-amz-user-agent", "aws-sdk-go/1.0.0 ua/2.1 os/"+runtime.GOOS+" lang/go md/ccl m/N,M,E")
	if credential.userID != "" {
		request.Header.Set("x-kiro-userid", credential.userID)
	}
	if visitorID := kiroPortalVisitorID(credential); visitorID != "" {
		request.Header.Set("x-kiro-visitorid", visitorID)
	}
	addKiroPortalCookies(request, credential, kiroPortalIDP(credential))

	response, err := service.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Kiro Web Portal ListAvailableModels: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read Kiro Web Portal ListAvailableModels response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &kiroUpstreamError{status: response.StatusCode, body: strings.TrimSpace(string(raw))}
	}
	const dataPrefix = "data:application/cbor;base64,"
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, dataPrefix) {
		encoded := trimmed[len(dataPrefix):]
		raw, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode Kiro Web Portal CBOR data URL: %w", err)
		}
	}
	var decoded kiroPortalModelsResponse
	if err := codec.NewDecoderBytes(raw, kiroCBORHandle).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode Kiro Web Portal ListAvailableModels response: %w", err)
	}
	return decoded.Models, nil
}

func parseKiroPortalMetadata(reader io.Reader) (map[string]string, error) {
	document, err := htmlparser.Parse(reader)
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]string)
	var visit func(*htmlparser.Node)
	visit = func(node *htmlparser.Node) {
		if node.Type == htmlparser.ElementNode && strings.EqualFold(node.Data, "meta") {
			name := ""
			value := ""
			for _, attribute := range node.Attr {
				switch strings.ToLower(attribute.Key) {
				case "name":
					name = strings.ToLower(strings.TrimSpace(attribute.Val))
				case "content", "value":
					if value == "" {
						value = strings.TrimSpace(attribute.Val)
					}
				}
			}
			if name != "" && value != "" {
				metadata[name] = value
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	return metadata, nil
}

func addKiroPortalCookies(request *http.Request, credential *kiroCredential, idp string) {
	for name, value := range map[string]string{
		"AccessToken":     credential.accessToken,
		"RefreshToken":    credential.refreshToken,
		"Idp":             idp,
		"UserId":          credential.userID,
		"kiro-visitor-id": kiroPortalVisitorID(credential),
	} {
		if value != "" {
			request.AddCookie(&http.Cookie{Name: name, Value: value})
		}
	}
}

func kiroPortalIDP(credential *kiroCredential) string {
	switch strings.ToLower(strings.TrimSpace(credential.provider)) {
	case "google":
		return "Google"
	case "github":
		return "Github"
	case "builderid", "builder-id", "aws":
		return "BuilderId"
	case "awsidc", "idc", "enterprise":
		return "AWSIdC"
	}
	switch strings.ToLower(strings.TrimSpace(credential.authMethod)) {
	case "builder-id":
		return "BuilderId"
	case "idc", "iam":
		return "AWSIdC"
	}
	return ""
}

func kiroPortalVisitorID(credential *kiroCredential) string {
	if credential.visitorID != "" {
		return credential.visitorID
	}
	machineID := credential.effectiveMachineID()
	if len(machineID) > 12 {
		machineID = machineID[:12]
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "-" + machineID
}

func (c *kiroModelCatalog) requestAvailableModels(ctx context.Context, service *kiroService, credential *kiroCredential, region string) ([]kiroAvailableModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(region), nil)
	if err != nil {
		return nil, err
	}
	machineID := credential.effectiveMachineID()
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Connection", "close")
	request.Header.Set("Authorization", "Bearer "+credential.accessToken)
	request.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.0 KiroIDE-"+kiroModelsAPIVersion+"-"+machineID)
	request.Header.Set("User-Agent", "aws-sdk-js/1.0.0 ua/2.1 os/"+runtime.GOOS+" lang/js md/nodejs#22.22.0 api/codewhispererruntime#1.0.0 m/N,E KiroIDE-"+kiroModelsAPIVersion+"-"+machineID)
	request.Header.Set("amz-sdk-invocation-id", uuidString())
	request.Header.Set("amz-sdk-request", "attempt=1; max=1")
	if strings.EqualFold(credential.authMethod, "external_idp") {
		request.Header.Set("tokentype", "EXTERNAL_IDP")
	}

	response, err := service.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Kiro ListAvailableModels: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read Kiro ListAvailableModels response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &kiroUpstreamError{status: response.StatusCode, body: strings.TrimSpace(string(body))}
	}
	var decoded kiroAvailableModelsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode Kiro ListAvailableModels response: %w", err)
	}
	return decoded.Models, nil
}

func kiroRESTRegionCandidates(_ *kiroCredential) []string {
	return []string{"us-east-1", "eu-central-1"}
}
