package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	kimiOAuthClientID  = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiMaxErrorBytes  = int64(1 << 20)
	kimiPlatformHeader = "CLIProxyAPI"
	kimiVersionHeader  = "ccl"
)

var (
	// kimiAPIBaseURL is the Kimi Code data plane base; callOnce appends
	// /chat/completions, yielding https://api.kimi.com/coding/v1/chat/completions
	// to match CPA's KimiAPIBaseURL + "/v1/chat/completions". A var (not const)
	// so tests can point it at a stub.
	kimiAPIBaseURL = "https://api.kimi.com/coding/v1"
	// kimiTokenURL is the OAuth refresh endpoint. A var so tests can stub it.
	kimiTokenURL = "https://auth.kimi.com/api/oauth/token"
)

// kimiOAuthAuthorizer resolves and refreshes a Kimi OAuth credential written by
// CPA's kimi authenticator during `ccl oauth kimi`. It reads the same fields
// CPA's executor reads (access_token/refresh_token/device_id) and refreshes
// against Kimi's token endpoint.
type kimiOAuthAuthorizer struct {
	path   string
	client *http.Client
	mu     sync.Mutex
}

type kimiOAuthCredential struct {
	metadata     map[string]any
	accessToken  string
	refreshToken string
	deviceID     string
	email        string
	expiresAt    time.Time
	disabled     bool
}

func (a *kimiOAuthAuthorizer) authorize(ctx context.Context, force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return "", err
	}
	if credential.disabled {
		return "", fmt.Errorf("Kimi credential %s is disabled", filepath.Base(a.path))
	}
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return "", err
		}
	}
	return credential.accessToken, nil
}

func (*kimiOAuthAuthorizer) isOAuth() bool { return true }

func (a *kimiOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: ProviderKimi, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: credential.metadata,
	}}
}

func (a *kimiOAuthAuthorizer) load() (*kimiOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Kimi credentials found; run `ccl oauth kimi` first")
		}
		return nil, fmt.Errorf("read Kimi credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Kimi credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != ProviderKimi {
		return nil, fmt.Errorf("credential %s is type %q, not Kimi", filepath.Base(a.path), credentialType)
	}
	disabled, _ := metadata["disabled"].(bool)
	return &kimiOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), deviceID: firstMetadataString(metadata, "device_id"),
		email: firstMetadataString(metadata, "email"), expiresAt: parseCodexExpiry(firstMetadataString(metadata, "expired")),
		disabled: disabled,
	}, nil
}

func (a *kimiOAuthAuthorizer) refresh(ctx context.Context, credential *kimiOAuthCredential) (*kimiOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Kimi credential %s has no refresh token", filepath.Base(a.path))
	}
	form := url.Values{
		"client_id": {kimiOAuthClientID}, "grant_type": {"refresh_token"},
		"refresh_token": {credential.refreshToken},
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, kimiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	for key, values := range kimiCommonHeaders(credential.deviceID, kimiAuthDeviceModel()) {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("refresh Kimi token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, kimiMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Kimi token refresh: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh Kimi token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		Email        string  `json:"email"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Kimi token refresh: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("refresh Kimi token: response has no access token")
	}
	credential.accessToken = token.AccessToken
	if token.RefreshToken != "" {
		credential.refreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		credential.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	if token.Email != "" {
		credential.email = token.Email
	}
	credential.metadata["type"] = ProviderKimi
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	if token.Email != "" {
		credential.metadata["email"] = token.Email
	}
	if !credential.expiresAt.IsZero() {
		credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	}
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=kimi_chat credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

func (a *kimiOAuthAuthorizer) decorateHeader(header http.Header) {
	deviceID := a.deviceIDLocked()
	if deviceID == "" {
		deviceID = kimiDeviceID()
	}
	header.Set("User-Agent", "CLIProxyAPI/"+kimiVersionHeader)
	for key, values := range kimiCommonHeaders(deviceID, kimiExecutorDeviceModel()) {
		for _, value := range values {
			header.Set(key, value)
		}
	}
}

// kimiCommonHeaders builds the Kimi device-identity headers CPA attaches to
// every request. deviceModel is passed in because the OAuth server
// (auth.kimi.com) and the API data plane (api.kimi.com/coding) report different
// device-model formats in CPA.
func kimiCommonHeaders(deviceID, deviceModel string) http.Header {
	header := make(http.Header)
	header.Set("X-Msh-Platform", kimiPlatformHeader)
	header.Set("X-Msh-Version", kimiVersionHeader)
	header.Set("X-Msh-Device-Name", kimiHostname())
	header.Set("X-Msh-Device-Model", deviceModel)
	header.Set("X-Msh-Device-Id", deviceID)
	return header
}

// deviceIDLocked reads the credential's persisted device_id under the same lock
// as authorize to avoid racing a concurrent refresh.
func (a *kimiOAuthAuthorizer) deviceIDLocked() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.load()
	if err != nil {
		return ""
	}
	return credential.deviceID
}

func kimiHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// kimiExecutorDeviceModel mirrors CPA executor's getKimiDeviceModel, the raw
// "<GOOS> <GOARCH>" format the data plane sends to api.kimi.com/coding.
func kimiExecutorDeviceModel() string {
	return runtime.GOOS + " " + runtime.GOARCH
}

// kimiAuthDeviceModel mirrors CPA auth's getDeviceModel, the friendly
// "macOS/Windows/Linux <arch>" format the OAuth server expects.
func kimiAuthDeviceModel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS " + runtime.GOARCH
	case "windows":
		return "Windows " + runtime.GOARCH
	case "linux":
		return "Linux " + runtime.GOARCH
	default:
		return runtime.GOOS + " " + runtime.GOARCH
	}
}

// kimiDeviceID returns a stable device ID matching kimi-cli's storage location,
// falling back to the same sentinel CPA uses when none is found.
func kimiDeviceID() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "cli-proxy-api-device"
	}
	var shareDir string
	switch runtime.GOOS {
	case "darwin":
		shareDir = filepath.Join(homeDir, "Library", "Application Support", "kimi")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(homeDir, "AppData", "Roaming")
		}
		shareDir = filepath.Join(appData, "kimi")
	default:
		shareDir = filepath.Join(homeDir, ".local", "share", "kimi")
	}
	if data, err := os.ReadFile(filepath.Join(shareDir, "device_id")); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "cli-proxy-api-device"
}

// normalizeKimiUpstreamModel maps a Kimi model ID onto its canonical upstream ID:
// it strips the "kimi-" prefix, drops a Claude Code "[1m]" context suffix, and
// remaps legacy K2.7 Code aliases to the official Kimi Code IDs. A trailing
// thinking suffix like "(1024)" is preserved, so "kimi-k3[1m](1024)" becomes
// "k3(1024)".
func normalizeKimiUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	modelName, suffix, hasSuffix := splitModelThinkingSuffix(model)
	base := strings.ToLower(strings.TrimSpace(modelName))
	if strings.HasSuffix(base, "[1m]") {
		base = strings.TrimSpace(base[:len(base)-len("[1m]")])
	}
	var normalized string
	switch base {
	case "kimi-k2.7-code", "k2.7-code", "kimi-for-coding", "for-coding":
		normalized = "kimi-for-coding"
	case "kimi-k2.7-code-highspeed", "k2.7-code-highspeed", "kimi-for-coding-highspeed", "for-coding-highspeed":
		normalized = "kimi-for-coding-highspeed"
	default:
		normalized = stripKimiPrefix(base)
	}
	if hasSuffix {
		return normalized + "(" + suffix + ")"
	}
	return normalized
}

// splitModelThinkingSuffix splits a trailing "(value)" suffix off a model ID,
// mirroring CPA's thinking.ParseSuffix. It only splits when the model ends with
// a closing parenthesis so IDs like "gemini-2.5-pro" pass through unchanged.
func splitModelThinkingSuffix(model string) (modelName, suffix string, hasSuffix bool) {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:lastOpen], model[lastOpen+1 : len(model)-1], true
}

func stripKimiPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "kimi-") {
		return model[len("kimi-"):]
	}
	return model
}

// normalizeKimiBody mirrors CPA's normalizeKimiToolMessageLinks: it links tool
// results to the preceding tool call when tool_call_id is missing, patches
// call_id onto tool_call_id, back-fills assistant reasoning_content before a
// tool call, and drops assistant messages with empty content. This is applied to
// the marshalled Chat Completions body after model normalization.
func normalizeKimiBody(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	type messagePatch struct {
		index int
		path  string
		value string
	}

	msgs := messages.Array()
	droppedMessages := make([]bool, len(msgs))
	patches := make([]messagePatch, 0)
	pending := make([]string, 0)
	dropped := 0
	latestReasoning := ""
	hasLatestReasoning := false

	removePending := func(id string) {
		for idx := range pending {
			if pending[idx] == id {
				pending = append(pending[:idx], pending[idx+1:]...)
				return
			}
		}
	}

	for msgIndex, msg := range msgs {
		if shouldDropKimiAssistantMessage(msg) {
			droppedMessages[msgIndex] = true
			dropped++
			continue
		}
		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			reasoning := msg.Get("reasoning_content")
			if reasoning.Exists() {
				reasoningText := reasoning.String()
				if strings.TrimSpace(reasoningText) != "" {
					latestReasoning = reasoningText
					hasLatestReasoning = true
				}
			}
			toolCalls := msg.Get("tool_calls")
			if toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
				if !reasoning.Exists() || strings.TrimSpace(reasoning.String()) == "" {
					patches = append(patches, messagePatch{
						index: msgIndex, path: "reasoning_content",
						value: fallbackKimiAssistantReasoning(msg, hasLatestReasoning, latestReasoning),
					})
				}
				for _, toolCall := range toolCalls.Array() {
					id := strings.TrimSpace(toolCall.Get("id").String())
					if id != "" {
						pending = append(pending, id)
					}
				}
			}
		case "tool":
			toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())
			if toolCallID == "" {
				toolCallID = strings.TrimSpace(msg.Get("call_id").String())
				if toolCallID != "" {
					patches = append(patches, messagePatch{index: msgIndex, path: "tool_call_id", value: toolCallID})
				}
			}
			if toolCallID == "" && len(pending) == 1 {
				toolCallID = pending[0]
				patches = append(patches, messagePatch{index: msgIndex, path: "tool_call_id", value: toolCallID})
			}
			if toolCallID != "" {
				removePending(toolCallID)
			}
		}
	}

	if dropped == 0 && len(patches) == 0 {
		return body, nil
	}

	var out []byte
	if dropped == 0 && len(patches) == 1 {
		patch := patches[0]
		path := fmt.Sprintf("messages.%d.%s", patch.index, patch.path)
		updated, err := sjson.SetBytes(body, path, patch.value)
		if err != nil {
			return body, fmt.Errorf("kimi: set %s: %w", path, err)
		}
		out = updated
	} else {
		messageItems := make([]string, 0, len(msgs)-dropped)
		patchIndex := 0
		for msgIndex, msg := range msgs {
			if droppedMessages[msgIndex] {
				continue
			}
			messageJSON := msg.Raw
			for patchIndex < len(patches) && patches[patchIndex].index == msgIndex {
				patch := patches[patchIndex]
				next, err := sjson.SetBytes([]byte(messageJSON), patch.path, patch.value)
				if err != nil {
					return body, fmt.Errorf("kimi: set %s: %w", patch.path, err)
				}
				messageJSON = string(next)
				patchIndex++
			}
			messageItems = append(messageItems, messageJSON)
		}
		updated, err := sjson.SetRawBytes(body, "messages", []byte(joinRawJSONStrings(messageItems)))
		if err != nil {
			return body, fmt.Errorf("kimi: rebuild messages: %w", err)
		}
		out = updated
	}
	return out, nil
}

// joinRawJSONStrings joins raw JSON values into a JSON array. It mirrors CPA's
// helps.JoinRawJSONStrings helper without taking a dependency on the SDK.
func joinRawJSONStrings(items []string) string {
	return "[" + strings.Join(items, ",") + "]"
}

func shouldDropKimiAssistantMessage(msg gjson.Result) bool {
	if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
		return false
	}
	if hasKimiToolCalls(msg) || hasKimiLegacyFunctionCall(msg) || hasKimiAssistantReasoning(msg) {
		return false
	}
	return isKimiAssistantContentEmpty(msg.Get("content"))
}

func hasKimiToolCalls(msg gjson.Result) bool {
	toolCalls := msg.Get("tool_calls")
	return toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0
}

func hasKimiLegacyFunctionCall(msg gjson.Result) bool {
	functionCall := msg.Get("function_call")
	if !functionCall.Exists() || functionCall.Type == gjson.Null {
		return false
	}
	if functionCall.IsObject() && strings.TrimSpace(functionCall.Raw) == "{}" {
		return false
	}
	return strings.TrimSpace(functionCall.Raw) != ""
}

func hasKimiAssistantReasoning(msg gjson.Result) bool {
	reasoning := msg.Get("reasoning_content")
	return reasoning.Exists() && strings.TrimSpace(reasoning.String()) != ""
}

func isKimiAssistantContentEmpty(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return true
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if !isKimiAssistantContentPartEmpty(part) {
			return false
		}
	}
	return true
}

func isKimiAssistantContentPartEmpty(part gjson.Result) bool {
	if !part.Exists() || part.Type == gjson.Null {
		return true
	}
	if part.Type == gjson.String {
		return strings.TrimSpace(part.String()) == ""
	}
	if !part.IsObject() {
		return false
	}
	if text := part.Get("text"); text.Exists() {
		return strings.TrimSpace(text.String()) == ""
	}
	if strings.TrimSpace(part.Get("type").String()) == "text" {
		return true
	}
	return strings.TrimSpace(part.Raw) == "{}"
}

func fallbackKimiAssistantReasoning(msg gjson.Result, hasLatest bool, latest string) string {
	if hasLatest && strings.TrimSpace(latest) != "" {
		return latest
	}
	content := msg.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			return text
		}
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return "[reasoning unavailable]"
}

// startKimiOAuth starts the CCL-owned Kimi Chat Completions data plane. Kimi
// speaks OpenAI Chat Completions with a handful of device-identity headers and a
// model-ID normalization pass, so it reuses the shared chat service with those
// two Kimi-specific hooks installed.
func startKimiOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &kimiOAuthAuthorizer{
		path: path,
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("Kimi credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Kimi credential %s has no access or refresh token", filepath.Base(path))
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Kimi Chat runtime requires at least one model")
	}
	runtime, err := startOpenAIChatRuntimeService(parent, kimiAPIBaseURL, routes, authorizer, func(service *chatCompletionsService) {
		service.normalizeModel = normalizeKimiUpstreamModel
		service.decorateHeader = authorizer.decorateHeader
		service.normalizeBody = normalizeKimiBody
	})
	if err != nil {
		return nil, err
	}
	runtime.listAuths = authorizer.listAuths
	LogInfof("runtime start oauth provider=kimi backend=kimi protocol=openai_chat port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(runtime.models))
	return runtime, nil
}
