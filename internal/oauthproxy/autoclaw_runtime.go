package oauthproxy

import (
	"context"
	"encoding/json"
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
	routes := autoClawModelRoutes(modelSpec)
	if len(routes) == 0 {
		for _, model := range AutoClawModelIDs() {
			routes = append(routes, runtimeModelRoute{Name: model, Alias: model})
		}
	}
	proxyRuntime, err := startOpenAIChatRuntimeService(parent, AutoClawOpenAIBaseURL(), routes, authorizer,
		func(service *chatCompletionsService) {
			addAutoClawDisplayAliases(service, routes)
			service.normalizeBody = normalizeAutoClawBody
			service.decorateHeader = authorizer.decorateHeader
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
	proxyRuntime.modelNames = autoClawModelDisplayNames(routes)
	LogInfof("runtime start autoclaw auth=oauth protocol=openai_chat endpoint=%q local_endpoint=%q model_count=%d",
		SafeLogEndpoint(AutoClawOpenAIBaseURL()), SafeLogEndpoint(proxyRuntime.Endpoint()), len(routes))
	return proxyRuntime, nil
}

func autoClawModelRoutes(modelSpec string) []runtimeModelRoute {
	configured := runtimeModelRoutes(modelSpec)
	if len(configured) == 0 {
		return nil
	}
	routes := make([]runtimeModelRoute, 0, len(configured))
	seen := make(map[string]bool, len(configured))
	for _, route := range configured {
		name := autoClawCanonicalModel(route.Name)
		key := strings.ToLower(name) + "\x00" + strings.ToLower(route.Alias)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		routes = append(routes, runtimeModelRoute{Name: name, Alias: route.Alias})
	}
	return routes
}

// addAutoClawDisplayAliases keeps Claude Code's readable catalog labels usable
// as request aliases. The launcher intentionally exposes names such as
// "Auto" and "GLM-5.3-Flash" in its model UI, while AutoClaw's managed API
// routes by IDs such as zai_auto and zai_glm-5.3-flash.
func addAutoClawDisplayAliases(service *chatCompletionsService, routes []runtimeModelRoute) {
	if service == nil {
		return
	}
	for _, route := range routes {
		technical := strings.TrimSpace(route.Name)
		if technical == "" {
			continue
		}
		for _, entry := range AutoClawModelCatalog() {
			if strings.EqualFold(entry.ID, technical) && strings.TrimSpace(entry.DisplayName) != "" {
				service.modelRoute[strings.ToLower(strings.TrimSpace(entry.DisplayName))] = technical
				break
			}
		}
	}
}

func autoClawCanonicalModel(model string) string {
	normalized := strings.TrimSpace(model)
	switch strings.ToLower(normalized) {
	case "glm-5.3":
		return "zaicoding_glm-5.3"
	case "glm-5.3-flash":
		return "zai_glm-5.3-flash"
	case "glm-5-turbo":
		return "zai_auto"
	}
	for _, id := range AutoClawModelIDs() {
		if strings.EqualFold(id, normalized) {
			return id
		}
	}
	return normalized
}

func autoClawModelDisplayNames(routes []runtimeModelRoute) map[string]string {
	result := make(map[string]string, len(routes))
	for _, route := range routes {
		for _, entry := range AutoClawModelCatalog() {
			if strings.EqualFold(entry.ID, route.Name) {
				result[route.Alias] = entry.DisplayName
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
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode AutoClaw Chat body: %w", err)
	}
	model, _ := body["model"].(string)
	body["model"] = autoClawBodyModel(model)
	// The shared Anthropic-to-Chat converter requests a usage-only final chunk
	// from every OpenAI-compatible backend. AutoClaw's own broker only adds
	// stream_options for its MiniMax/Moonshot routes; its managed ZAI routes
	// forward the body without this field. Match that working ZCode shape.
	delete(body, "stream_options")
	return json.Marshal(body)
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
