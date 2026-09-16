package oauthproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// startAutoClawOAuth runs a CCL-owned Anthropic-to-OpenAI adapter. Claude Code
// still talks to the usual local /v1/messages endpoint, while CCL sends the
// converted request to AutoClaw's managed OpenAI Chat proxy with the exact
// X-Authorization/X-Request-Model contract used by the desktop app.
func startAutoClawOAuth(parent context.Context, _ string, modelSpec, credentialFile string) (*Runtime, error) {
	authorizer, err := newAutoClawOAuthAuthorizer(credentialFile)
	if err != nil {
		return nil, err
	}
	contract, contractFromRuntime := autoClawEffectiveContract()
	routes := autoClawModelRoutesWithCatalog(modelSpec, contract.models)
	managedEndpoint := strings.TrimRight(autoclawAPIOrigin, "/") + "/autoclaw-proxy/proxy/autoclaw"
	if contract.baseURL != "" {
		managedEndpoint = contract.baseURL
	}
	proxyRuntime, err := startOpenAIChatRuntimeService(parent, managedEndpoint, routes, authorizer,
		func(service *chatCompletionsService) {
			// The generated contract is the advertised catalog. Configured stale
			// routes remain accepted as aliases long enough for the user to remap
			// them, but they must not masquerade as currently available models.
			service.models = autoClawCatalogIDs(contract.models)
			addAutoClawDisplayAliasesWithCatalog(service, routes, contract.models)
			service.normalizeRequest = func(converted *chatCompletionsConvertedRequest) error {
				return normalizeAutoClawRequest(converted, contract)
			}
			service.decorateHeader = authorizer.decorateHeader
			// A managed-plan invocation may have been accepted before its broker
			// returns an ambiguous 5xx (for example "parse response failed"). Do
			// not replay billable POSTs invisibly; surface the first failure and let
			// the caller decide whether to retry.
			service.fastRetry = false
			// AutoClaw's managed broker is implemented with Node fetch/undici,
			// which currently sends HTTP/1.1. Match that transport when CCL
			// calls the same endpoint; some WAF rules distinguish HTTP/2.
			service.client.Transport = &http.Transport{
				Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false,
				ResponseHeaderTimeout: 90 * time.Second,
			}
		})
	if err != nil {
		return nil, err
	}
	proxyRuntime.listAuths = authorizer.listAuths
	proxyRuntime.modelNames = autoClawModelDisplayNamesWithCatalog(routes, contract.models)
	proxyRuntime.catalogFallback = !contractFromRuntime
	LogInfof("runtime start autoclaw auth=oauth protocol=openai_chat endpoint=%q local_endpoint=%q model_count=%d",
		SafeLogEndpoint(managedEndpoint), SafeLogEndpoint(proxyRuntime.Endpoint()), len(contract.models))
	return proxyRuntime, nil
}

func autoClawModelRoutes(modelSpec string) []runtimeModelRoute {
	contract, _ := autoClawEffectiveContract()
	return autoClawModelRoutesWithCatalog(modelSpec, contract.models)
}

func autoClawModelRoutesWithCatalog(modelSpec string, catalog []autoclawModelDefinition) []runtimeModelRoute {
	configured := runtimeModelRoutes(modelSpec)
	routes := make([]runtimeModelRoute, 0, len(configured)+len(catalog))
	seen := make(map[string]bool, len(configured)+len(catalog))
	add := func(name, alias string) {
		key := strings.ToLower(name) + "\x00" + strings.ToLower(alias)
		if name == "" || alias == "" || seen[key] {
			return
		}
		seen[key] = true
		routes = append(routes, runtimeModelRoute{Name: name, Alias: alias})
	}
	for _, route := range configured {
		name := autoClawCanonicalModelWithCatalog(route.Name, catalog)
		add(name, route.Alias)
	}
	for _, model := range catalog {
		add(model.id, model.id)
	}
	return routes
}

func autoClawCatalogIDs(catalog []autoclawModelDefinition) []string {
	ids := make([]string, 0, len(catalog))
	for _, model := range catalog {
		if id := strings.TrimSpace(model.id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// addAutoClawDisplayAliases keeps Claude Code's readable catalog labels usable
// as request aliases. The launcher intentionally exposes names such as
// "Auto" and "GLM-5.3-Flash" in its model UI, while AutoClaw's managed API
// routes by IDs such as zai_auto and zai_glm-5.3-flash.
func addAutoClawDisplayAliases(service *chatCompletionsService, routes []runtimeModelRoute) {
	contract, _ := autoClawEffectiveContract()
	addAutoClawDisplayAliasesWithCatalog(service, routes, contract.models)
}

func addAutoClawDisplayAliasesWithCatalog(service *chatCompletionsService, routes []runtimeModelRoute, catalog []autoclawModelDefinition) {
	if service == nil {
		return
	}
	for _, route := range routes {
		technical := strings.TrimSpace(route.Name)
		if technical == "" {
			continue
		}
		for _, entry := range catalog {
			if strings.EqualFold(entry.id, technical) && strings.TrimSpace(entry.name) != "" {
				service.modelRoute[strings.ToLower(strings.TrimSpace(entry.name))] = technical
				break
			}
		}
	}
}

func autoClawCanonicalModel(model string) string {
	contract, _ := autoClawEffectiveContract()
	return autoClawCanonicalModelWithCatalog(model, contract.models)
}

func autoClawCanonicalModelWithCatalog(model string, catalog []autoclawModelDefinition) string {
	normalized := strings.TrimSpace(model)
	switch strings.ToLower(normalized) {
	case "glm-5.3":
		return "zaicoding_glm-5.3"
	case "glm-5.3-flash":
		return "zai_glm-5.3-flash"
	case "glm-5-turbo":
		return "zai_auto"
	}
	for _, entry := range catalog {
		if strings.EqualFold(entry.id, normalized) {
			return entry.id
		}
	}
	return normalized
}

func autoClawModelDisplayNames(routes []runtimeModelRoute) map[string]string {
	contract, _ := autoClawEffectiveContract()
	return autoClawModelDisplayNamesWithCatalog(routes, contract.models)
}

func autoClawModelDisplayNamesWithCatalog(routes []runtimeModelRoute, catalog []autoclawModelDefinition) map[string]string {
	result := make(map[string]string, len(routes))
	for _, route := range routes {
		for _, entry := range catalog {
			if strings.EqualFold(entry.id, route.Name) {
				result[route.Alias] = entry.name
				break
			}
		}
	}
	return result
}

// normalizeAutoClawBody maps the desktop route ID to the body model accepted
// by the managed proxy. AutoClaw removes a leading lowercase provider prefix:
// zai_auto -> auto, zaicoding_glm-5.3 -> glm-5.3.
func normalizeAutoClawBody(raw []byte) ([]byte, error) {
	contract, _ := autoClawEffectiveContract()
	var initial map[string]any
	if err := json.Unmarshal(raw, &initial); err != nil {
		return nil, fmt.Errorf("decode AutoClaw Chat body: %w", err)
	}
	route, _ := initial["model"].(string)
	converted := &chatCompletionsConvertedRequest{
		anthropicAdapterRequest: anthropicAdapterRequest{upstreamModel: route},
		body:                    raw,
		model:                   route,
	}
	if err := normalizeAutoClawRequest(converted, contract); err != nil {
		return nil, err
	}
	return converted.body, nil
}

// normalizeAutoClawRequest applies the compatibility metadata emitted by
// AutoClaw's own OpenClaw runtime. It keeps the routing header and body model in
// sync, sends image-bearing requests through AutoClaw's configured image model,
// and supplies the empty reasoning_content field required by its Auto/DeepSeek
// adapters when replaying assistant history.
func normalizeAutoClawRequest(converted *chatCompletionsConvertedRequest, contract autoClawLocalContract) error {
	if converted == nil {
		return errors.New("normalize AutoClaw request: request is nil")
	}
	var body map[string]any
	if err := json.Unmarshal(converted.body, &body); err != nil {
		return fmt.Errorf("decode AutoClaw Chat body: %w", err)
	}
	route := autoClawCanonicalModelWithCatalog(converted.model, contract.models)
	if route == "" {
		route, _ = body["model"].(string)
		route = autoClawCanonicalModelWithCatalog(route, contract.models)
	}
	if autoClawBodyHasImage(body) && !autoClawModelSupportsImage(contract.models, route) {
		imageModel := autoClawPreferredImageModel(contract)
		if imageModel == "" {
			return fmt.Errorf("AutoClaw model %q does not accept images and the managed catalog has no image-capable fallback", route)
		}
		route = imageModel
	}
	converted.model = route
	converted.upstreamModel = route
	body["model"] = autoClawBodyModel(route)
	if definition, ok := autoClawModelDefinition(contract.models, route); ok && definition.reasoning && definition.requiresAssistantReasoningContent {
		for _, rawMessage := range sliceValue(body["messages"]) {
			message := mapValue(rawMessage)
			if !strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "assistant") {
				continue
			}
			if _, exists := message["reasoning_content"]; !exists {
				message["reasoning_content"] = ""
			}
		}
	}
	// The shared Anthropic-to-Chat converter requests a usage-only final chunk
	// from every OpenAI-compatible backend. AutoClaw's own broker only adds
	// stream_options for its MiniMax/Moonshot routes; its managed ZAI routes
	// forward the body without this field. Match that working ZCode shape.
	delete(body, "stream_options")
	normalized, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode AutoClaw Chat body: %w", err)
	}
	converted.body = normalized
	return nil
}

func autoClawBodyHasImage(body map[string]any) bool {
	for _, rawMessage := range sliceValue(body["messages"]) {
		message := mapValue(rawMessage)
		for _, rawPart := range sliceValue(message["content"]) {
			part := mapValue(rawPart)
			switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
			case "image_url", "input_image", "image":
				return true
			}
		}
	}
	return false
}

func autoClawBodyModel(routeModel string) string {
	routeModel = strings.TrimSpace(routeModel)
	if separator := strings.IndexByte(routeModel, '_'); separator > 0 {
		prefix := routeModel[:separator]
		lowercase := true
		for _, character := range prefix {
			if character < 'a' || character > 'z' {
				lowercase = false
				break
			}
		}
		if lowercase {
			return routeModel[separator+1:]
		}
	}
	return routeModel
}
