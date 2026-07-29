package cmd

import (
	"bytes"

	"charm.land/lipgloss/v2"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/cloudsync"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

// doctorProbeTimeout bounds each connectivity probe issued by `ccl doctor`.
const doctorProbeTimeout = 5 * time.Second

var doctorCmd = newDoctorCommand()

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Show provider status and check connectivity",
		Long: `Show environment prerequisites, active provider status, and connectivity.

For normal API-key providers it prints the Review & Apply essentials
(endpoint, masked key, protocol, runtime, slot mappings). For OAuth groups
it also summarizes CPA credential health: healthy, invalid/missing, and
quota-exhausted members.

Model pool (provider.Model) is an optional candidate list used by ccl set/map
for discovery and bulk checks. Claude Code actually uses the slot mappings
(Opus/Sonnet/Haiku/Custom/Subagent). An empty pool with filled slots is normal.

Note: root "ccl status" is cloud sync status (see "ccl cloud status"), not this command.
For live request failures enable "ccl debug on" and check the log path printed
when the Claude session ends (default /tmp/ccl-debug.log).
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context())
		},
	}
}

func doctorHeader(title string) {
	fmt.Println(titleStyle.Foreground(colorAccent).Render(title))
	fmt.Println(grayText.Render(strings.Repeat("─", 44)))
}

func doctorSection(title string) {
	fmt.Println()
	fmt.Println(titleStyle.Foreground(colorSecondary).Render("▸ " + title))
}

func doctorKV(label, value string) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "-"
	}
	fmt.Printf("  %s %s\n",
		grayText.Render(fmt.Sprintf("%-18s", label)),
		cyanText.Render(value),
	)
}

func doctorOK(msg string) {
	fmt.Printf("  %s %s\n", availableStyle.Render("✓"), msg)
}

func doctorWarn(msg string) {
	fmt.Printf("  %s %s\n",
		lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("!"),
		msg,
	)
}

func doctorErr(msg string) {
	fmt.Printf("  %s %s\n", unavailableStyle.Render("✗"), msg)
}

func doctorInfo(msg string) {
	fmt.Printf("  %s %s\n", grayText.Render("•"), msg)
}

func doctorHint(msg string) {
	fmt.Println(grayText.Render("  ↳ " + msg))
}

func runDoctor(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	doctorHeader("ccl Doctor")

	doctorSection("Environment")

	// 1. Check Node.js
	nodePath, err := exec.LookPath("node")
	if err != nil {
		doctorInfo("Node.js: not installed (not required by newer native Claude Code binaries)")
	} else {
		doctorOK("Node.js: " + nodePath)
	}

	// 2. Check Claude CLI
	claudeInstalled := IsInstalled()
	if !claudeInstalled {
		doctorErr("Claude Code CLI: not installed or not in PATH")
		// Prompt to install automatically
		err := AutoInstall()
		if err != nil {
			doctorErr(fmt.Sprintf("Auto-installation failed: %v", err))
			doctorHint("Install manually: https://code.claude.com/")
		}
	} else {
		claudePath, _ := exec.LookPath("claude")
		doctorOK("Claude Code CLI: " + claudePath)
	}

	// 3. Check Configuration File
	cfg, err := config.Load()
	if err != nil {
		doctorErr(fmt.Sprintf("Config: %v", err))
		return nil
	}
	doctorOK("Config: " + config.ConfigPath())
	printCloudSyncDiagnostics()

	// 4. Check Active Provider
	if cfg.ActiveProvider == "" {
		doctorSection("Provider")
		doctorErr("No active provider. Use `ccl set` or `ccl use`.")
		return nil
	}
	doctorSection("Provider · " + cfg.ActiveProvider)

	p, ok := cfg.Providers[cfg.ActiveProvider]
	if !ok {
		doctorErr(fmt.Sprintf("Selected provider %q does not exist in config", cfg.ActiveProvider))
		return nil
	}

	printDoctorProviderIdentity(p)
	groupValid := true
	if p.AuthGroup != "" {
		groupValid = printDoctorGroupValidation(cfg, p)
	}
	printDoctorProviderDetails(p)
	printProviderExperienceWarnings(p)

	configuredProvider := p
	if !groupValid {
		printDoctorGroupHealth(cfg, p, nil)
		doctorHint("Run `ccl oauth sync` to reconcile group members with ~/.ccl/auth.")
		return nil
	}

	p, runtime, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		if configuredProvider.AuthGroup != "" || configuredProvider.OAuthProvider != "" {
			printDoctorGroupHealth(cfg, configuredProvider, nil)
		}
		doctorSection("Connectivity")
		doctorErr(err.Error())
		return nil
	}
	defer cleanup()
	if configuredProvider.AuthGroup != "" || configuredProvider.OAuthProvider != "" {
		printDoctorGroupHealth(cfg, configuredProvider, runtime)
	}
	printDoctorContextBudget(p, configuredProvider)

	// 5. Test Endpoint reachability and API Authentication key
	if p.Endpoint != "" {
		endpointReachable := checkDoctorConnectivity(ctx, p)

		// 6. Validate configured models with concurrent API calls and reorder (available first)
		if endpointReachable && p.Model != "" {
			configuredModels := parseModelList(p.Model)
			if len(configuredModels) > 0 {
				doctorSection("Model verification")
				availableSet := testModelsConcurrently(ctx, configuredModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
				available, unavailable := classifyModels(configuredModels, availableSet)
				doctorKV("Summary", modelVerificationSummary(available, unavailable))
				if len(unavailable) > 0 {
					doctorHint("Run `ccl models` to inspect individual model results.")
				}

				// Reorder and save: available first, unavailable last
				reordered := append(available, unavailable...)
				newModel := strings.Join(reordered, ",")
				if newModel != p.Model {
					configuredProvider.Model = newModel
					cfg.Providers[cfg.ActiveProvider] = configuredProvider
					if err := config.Save(cfg); err != nil {
						doctorErr(fmt.Sprintf("Failed to save reordered models: %v", err))
					} else {
						doctorOK("Config updated: available models prioritized.")
					}
				}
			}
		}
	}

	return nil
}

// doctorModelContextWindows collects the context window each mapped model
// advertises. Subscription runtimes only expose windows through the Codex client
// catalog, so that shape is tried first and the plain OpenAI list second.
// printDoctorContextBudget reports the context limits the session will run with
// and where they came from.
//
// A compact threshold above the real window is silently broken: Claude Code keeps
// growing the conversation and the upstream rejects the request with
// context_length_exceeded (surfaced as HTTP 400) before auto-compact ever runs.
// For subscription providers ccl now derives the limits from the backend catalog,
// so this section mainly shows which numbers won.
//
// runtimeProvider carries the live endpoint/key of the embedded runtime;
// configured carries the user's ccl config, which owns the compact env.
func printDoctorContextBudget(runtimeProvider, configured provider.Provider) {
	doctorSection("Context budget")

	maxContext := parseDoctorTokenEnv(configured.Env[maxContextTokensEnv])
	compactWindow := parseDoctorTokenEnv(configured.Env[autoCompactWindowEnv])
	windows, source := claude.AdvertisedContextWindows(runtimeProvider.Endpoint, runtimeProvider.APIKey)
	smallest, smallestModel, unknown := smallestMappedWindow(configured, windows)
	managed := strings.TrimSpace(configured.OAuthProvider) != "" && smallest > 0

	oauthproxy.Debugf("doctor context budget provider=%q configured_max_context=%d configured_compact=%d catalog=%q models=%d smallest=%d smallest_model=%q managed=%t",
		configured.Name, maxContext, compactWindow, source, len(windows), smallest, smallestModel, managed)

	if managed {
		// The launcher overrides the stored preset with these values.
		doctorKV("Managed by", source+" (subscription backend)")
		doctorKV("Assumed context", formatTokenCount(smallest)+" from "+smallestModel)
		doctorKV("Auto-compact at", formatTokenCount(claude.ManagedCompactWindow(smallest)))
	} else {
		doctorKV("Assumed context", doctorTokenLabel(maxContext, "Claude Code default"))
		doctorKV("Auto-compact at", doctorTokenLabel(compactWindow, "Claude Code default"))
	}
	if len(windows) == 0 {
		doctorInfo("Backend does not advertise context windows; ccl uses the configured preset")
		return
	}
	if !managed {
		doctorKV("Window source", source)
	}

	for _, slot := range provider.SlotModels(configured) {
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			doctorKV(slot.Slot+" window", fmt.Sprintf("%s (%s)", formatTokenCount(window), slot.Model))
		}
	}
	if unknown > 0 {
		doctorInfo(fmt.Sprintf("%d mapped model(s) are absent from the catalog; their window is unknown", unknown))
	}
	if smallest == 0 {
		return
	}
	if managed {
		doctorOK("Context and compact settings follow the backend; the ccl preset is read-only for this provider")
		if maxContext > smallest {
			doctorInfo(fmt.Sprintf("Stored preset (%s) is larger than the advertised window and is ignored",
				formatTokenCount(maxContext)))
		}
		return
	}

	threshold, thresholdLabel := effectiveCompactThreshold(maxContext, compactWindow)
	if threshold == 0 {
		doctorOK(fmt.Sprintf("No ccl context override; Claude Code follows the advertised window (smallest %s)", formatTokenCount(smallest)))
		return
	}
	if threshold > smallest {
		doctorErr(fmt.Sprintf("The %s (%s) is above the %s window of %s",
			thresholdLabel, formatTokenCount(threshold), smallestModel, formatTokenCount(smallest)))
		doctorHint("Requests are rejected with context_length_exceeded (HTTP 400) before Claude Code auto-compacts")
		doctorHint("Run `ccl set` → Compact and pick a preset at or below the advertised window")
		return
	}
	doctorOK(fmt.Sprintf("Compact threshold %s fits the smallest mapped window %s",
		formatTokenCount(threshold), formatTokenCount(smallest)))
}

// effectiveCompactThreshold reports the token count Claude Code will actually
// grow to, and a label for it. The absolute auto-compact window wins when set;
// otherwise the assumed context size governs. Zero means ccl set no override and
// Claude Code follows its own defaults.
func effectiveCompactThreshold(maxContext, compactWindow int) (int, string) {
	if compactWindow > 0 {
		return compactWindow, "auto-compact threshold"
	}
	if maxContext > 0 {
		return maxContext, "assumed context size"
	}
	return 0, ""
}

// smallestMappedWindow returns the smallest advertised context window across the
// provider's mapped slots, the model it belongs to, and how many mapped models
// the catalog did not report a window for.
func smallestMappedWindow(p provider.Provider, windows map[string]int) (smallest int, model string, unknown int) {
	for _, slot := range provider.SlotModels(p) {
		window, ok := windows[strings.ToLower(slot.Model)]
		if !ok || window <= 0 {
			unknown++
			continue
		}
		if smallest == 0 || window < smallest {
			smallest, model = window, slot.Model
		}
	}
	return smallest, model, unknown
}

func parseDoctorTokenEnv(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func doctorTokenLabel(tokens int, fallback string) string {
	if tokens == 0 {
		return fallback
	}
	return formatTokenCount(tokens)
}

// formatTokenCount renders a token count as a compact K/M label plus the exact
// number, so a warning is both readable and precise.
func formatTokenCount(tokens int) string {
	switch {
	case tokens >= 1_000_000 && tokens%1_000_000 == 0:
		return fmt.Sprintf("%dM (%d)", tokens/1_000_000, tokens)
	case tokens >= 1_000 && tokens%1_000 == 0:
		return fmt.Sprintf("%dK (%d)", tokens/1_000, tokens)
	default:
		return strconv.Itoa(tokens)
	}
}

// checkDoctorConnectivity probes the provider endpoint and reports whether it is
// reachable and authenticated. It lives in its own function so every response
// body and context is released here instead of at the end of the whole run.
func checkDoctorConnectivity(ctx context.Context, p provider.Provider) bool {
	doctorSection("Connectivity")
	doctorInfo("Checking endpoint and credentials...")
	client := &http.Client{Timeout: doctorProbeTimeout}

	modelsURL := protocol.NormalizeOpenAIModelsURL(p.Endpoint)
	if provider.IsAnthropicType(p.Type) {
		modelsURL = protocol.NormalizeAnthropicModelsURL(p.Endpoint)
	}

	status, err := doctorProbeEndpoint(ctx, client, modelsURL, p)
	if err != nil {
		doctorErr(fmt.Sprintf("Endpoint is unreachable: %v", err))
		return false
	}
	switch {
	case status == http.StatusOK:
		doctorOK(fmt.Sprintf("Connected and verified (HTTP %d)", status))
		return true
	case status == http.StatusUnauthorized:
		doctorErr(fmt.Sprintf("Authentication failed (HTTP %d). Verify the API key.", status))
		return false
	case status == http.StatusForbidden || status == http.StatusNotFound:
		// Fallback strategy if GET models returns 404 or 403 on third-party proxies.
		fallbackStatus, fallbackErr := doctorProbeEndpoint(ctx, client, p.Endpoint, p)
		if fallbackErr != nil {
			doctorErr(fmt.Sprintf("Models discovery returned HTTP %d; base endpoint fallback is unreachable: %v", status, fallbackErr))
			return false
		}
		if fallbackStatus == http.StatusUnauthorized || fallbackStatus == http.StatusForbidden {
			doctorErr(fmt.Sprintf("Authentication failed. Base endpoint returned HTTP %d. Verify the API key.", fallbackStatus))
			return false
		}
		doctorOK(fmt.Sprintf("Connected and verified (HTTP %d, models discovery bypassed)", status))
		return true
	default:
		doctorWarn(fmt.Sprintf("Connected, but returned unexpected status (HTTP %d)", status))
		return false
	}
}

// doctorProbeEndpoint issues one authenticated GET and returns its status code.
func doctorProbeEndpoint(ctx context.Context, client *http.Client, url string, p provider.Provider) (int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	setProviderAuthHeaders(request, p)
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
	return response.StatusCode, nil
}

func printCloudSyncDiagnostics() {
	report := cloudsync.DiagnoseLocal()
	doctorSection("Cloud sync")
	for _, check := range report.Checks {
		switch check.Level {
		case "ok":
			doctorOK(check.Message)
		case "warning":
			doctorWarn(check.Message)
		case "error":
			doctorErr(check.Message)
		default:
			doctorInfo(check.Message)
		}
	}
}

type doctorGroupValidation struct {
	Name              string
	OAuthProvider     string
	CredentialType    string
	ConfiguredMembers int
	AvailableMembers  int
	HealthyMembers    int
	InvalidMembers    int
	QuotaMembers      int
	DisabledMembers   int
	Problems          []string
}

// validateDoctorAuthGroup validates the group definition against the current
// one-level auth directory. Group identity comes from Provider.AuthGroup; the
// provider's display name and group- prefix are deliberately irrelevant.
func validateDoctorAuthGroup(cfg *provider.Config, p provider.Provider) doctorGroupValidation {
	result := doctorGroupValidation{Name: strings.TrimSpace(p.AuthGroup)}
	group, ok := cfg.AuthGroups[result.Name]
	if !ok {
		result.Problems = append(result.Problems, fmt.Sprintf("group definition %q is missing", result.Name))
		return result
	}
	result.OAuthProvider = group.OAuthProvider
	result.ConfiguredMembers = len(group.Credentials)

	backend, err := oauthproxy.BackendProvider(group.OAuthProvider)
	if err != nil {
		result.Problems = append(result.Problems, fmt.Sprintf("group has unsupported OAuth backend %q", group.OAuthProvider))
	} else {
		result.CredentialType = backend
	}
	if strings.TrimSpace(p.OAuthProvider) == "" {
		result.Problems = append(result.Problems, "group provider has no OAuth backend")
	} else if backend != "" {
		providerBackend, providerErr := oauthproxy.BackendProvider(p.OAuthProvider)
		if providerErr != nil || !strings.EqualFold(providerBackend, backend) {
			result.Problems = append(result.Problems,
				fmt.Sprintf("provider backend %q does not match group credential type %q", p.OAuthProvider, backend))
		}
	}
	if len(group.Credentials) == 0 {
		result.Problems = append(result.Problems, "group contains no credential files")
		return result
	}

	credentials, listErr := oauthproxy.ListCredentials()
	if listErr != nil {
		result.Problems = append(result.Problems, fmt.Sprintf("cannot scan auth directory: %v", listErr))
		return result
	}
	byFile := make(map[string]oauthproxy.CredentialInfo, len(credentials))
	for _, credential := range credentials {
		byFile[strings.ToLower(credential.FileName)] = credential
	}
	seen := make(map[string]bool, len(group.Credentials))
	for _, file := range group.Credentials {
		file = strings.TrimSpace(file)
		key := strings.ToLower(file)
		if file == "" {
			result.InvalidMembers++
			result.Problems = append(result.Problems, "group contains an empty credential filename")
			continue
		}
		if seen[key] {
			result.InvalidMembers++
			result.Problems = append(result.Problems, fmt.Sprintf("credential %q is listed more than once", file))
			continue
		}
		seen[key] = true
		credential, exists := byFile[key]
		if !exists {
			result.InvalidMembers++
			result.Problems = append(result.Problems,
				fmt.Sprintf("credential %q is missing, invalid, or has an unsupported type", file))
			continue
		}
		if backend != "" && !strings.EqualFold(credential.Backend, backend) {
			result.InvalidMembers++
			result.Problems = append(result.Problems,
				fmt.Sprintf("credential %q has type %q; group requires %q", file, credential.Backend, backend))
			continue
		}
		result.AvailableMembers++
		if credential.Disabled {
			result.DisabledMembers++
			result.InvalidMembers++
			result.Problems = append(result.Problems, fmt.Sprintf("credential %q is disabled", file))
			continue
		}
		if credential.QuotaExceeded {
			result.QuotaMembers++
			continue
		}
		if credential.Unavailable {
			result.InvalidMembers++
			continue
		}
		result.HealthyMembers++
	}
	return result
}

func printDoctorGroupValidation(cfg *provider.Config, p provider.Provider) bool {
	result := validateDoctorAuthGroup(cfg, p)
	doctorKV("Group", result.Name)
	if result.OAuthProvider != "" {
		if result.CredentialType != "" {
			doctorKV("Group backend", fmt.Sprintf("%s (credential type %s)", result.OAuthProvider, result.CredentialType))
		} else {
			doctorKV("Group backend", result.OAuthProvider)
		}
	}
	doctorKV("Group members", fmt.Sprintf("%d configured · %d available",
		result.ConfiguredMembers, result.AvailableMembers))
	if len(result.Problems) == 0 {
		doctorOK("Group configuration is valid and homogeneous")
		return true
	}
	for _, problem := range result.Problems {
		doctorErr("Group: " + problem)
	}
	return false
}

// testModelsConcurrently tests multiple models in batches of 50 concurrent workers.
// Each worker sends a lightweight provider-specific POST to verify the model works.
// Returns a set of model IDs that passed the test.
func testModelsConcurrently(ctx context.Context, models []string, endpoint, apiKey, providerType, anthropicAuth string) map[string]bool {
	if ctx == nil {
		ctx = context.Background()
	}
	const batchSize = 50
	const requestTimeout = 10 * time.Second

	available := make(map[string]bool)
	var mu sync.Mutex
	var completed, okCount, failCount int64
	total := int64(len(models))

	doctorInfo(fmt.Sprintf("Checking %d model(s)...", total))
	defer func() {
		c := atomic.LoadInt64(&completed)
		o := atomic.LoadInt64(&okCount)
		f := atomic.LoadInt64(&failCount)
		doctorKV("Checked", fmt.Sprintf("%d/%d · %d available · %d unavailable", c, total, o, f))
	}()

	for start := 0; start < len(models); start += batchSize {
		end := start + batchSize
		if end > len(models) {
			end = len(models)
		}
		batch := models[start:end]

		if ctx.Err() != nil {
			break
		}
		var wg sync.WaitGroup
		for _, model := range batch {
			wg.Add(1)
			go func(m string) {
				defer wg.Done()
				ok := testSingleModelContext(ctx, m, endpoint, apiKey, providerType, anthropicAuth, requestTimeout)
				if ok {
					mu.Lock()
					available[m] = true
					mu.Unlock()
					atomic.AddInt64(&okCount, 1)
				} else {
					atomic.AddInt64(&failCount, 1)
				}
				atomic.AddInt64(&completed, 1)
			}(model)
		}
		wg.Wait()
	}
	return available
}

func testSingleModelContext(ctx context.Context, model, endpoint, apiKey, providerType, anthropicAuth string, timeout time.Duration) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	if provider.IsAnthropicType(providerType) {
		if strings.TrimSpace(anthropicAuth) == "" {
			anthropicAuth = "x-api-key"
		}
		return testSingleAnthropicModelWithAuthContext(ctx, model, endpoint, apiKey, anthropicAuth, timeout)
	}
	if provider.IsOpenAIResponsesType(providerType) {
		return testSingleOpenAIResponsesModelContext(ctx, model, endpoint, apiKey, timeout)
	}
	return testSingleOpenAIModelContext(ctx, model, endpoint, apiKey, timeout)
}

// probeModel sends one minimal completion request and reports whether the
// upstream accepted it. The OpenAI and Anthropic probes differ only in URL,
// payload and auth headers.
func probeModel(parent context.Context, url string, payload map[string]any, headers map[string]string, timeout time.Duration) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func testSingleOpenAIModelContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	return probeModel(parent, buildChatURL(endpoint), map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	}, map[string]string{"Authorization": "Bearer " + apiKey}, timeout)
}

func testSingleAnthropicModelWithAuthContext(parent context.Context, model, endpoint, apiKey, authStyle string, timeout time.Duration) bool {
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	if strings.EqualFold(authStyle, "bearer") {
		headers["Authorization"] = "Bearer " + apiKey
	} else {
		headers["x-api-key"] = apiKey
	}
	return probeModel(parent, buildAnthropicMessagesURL(endpoint), map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	}, headers, timeout)
}

func testSingleOpenAIResponsesModelContext(ctx context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	return protocol.ProbeOpenAIResponsesSupportContext(ctx, endpoint, apiKey, model, timeout)
}

// buildChatURL constructs a chat completions endpoint URL from a provider endpoint.
func buildChatURL(endpoint string) string {
	return protocol.NormalizeOpenAIChatCompletionsURL(endpoint)
}

func buildAnthropicMessagesURL(endpoint string) string {
	return protocol.NormalizeAnthropicMessagesURL(endpoint)
}

// classifyModels splits configured models into available and unavailable slices,
// preserving original relative order within each group.
func classifyModels(configured []string, availableSet map[string]bool) (available, unavailable []string) {
	if len(availableSet) == 0 {
		unavailable = configured
		return
	}
	for _, m := range configured {
		if availableSet[m] {
			available = append(available, m)
		} else {
			unavailable = append(unavailable, m)
		}
	}
	return
}

func modelVerificationSummary(available, unavailable []string) string {
	return fmt.Sprintf("%d available · %d unavailable", len(available), len(unavailable))
}

func printDoctorGroupHealth(cfg *provider.Config, p provider.Provider, runtime *oauthproxy.Runtime) {
	result := validateDoctorAuthGroup(cfg, p)
	doctorSection("CPA accounts")

	if runtime == nil {
		// Offline file scan only — used when runtime failed to start or config is invalid.
		doctorKV("Source", "offline files (~/.ccl/auth)")
		doctorKV("Configured", fmt.Sprintf("%d", result.ConfiguredMembers))
		doctorKV("Present", fmt.Sprintf("%d", result.HealthyMembers))
		doctorKV("Invalid", fmt.Sprintf("%d", result.InvalidMembers))
		doctorKV("Quota flagged", fmt.Sprintf("%d", result.QuotaMembers))
		if result.DisabledMembers > 0 {
			doctorKV("Disabled", fmt.Sprintf("%d", result.DisabledMembers))
		}
		printDoctorAuthMetadataCounts(countOfflineAuthMetadata(cfg, p))
		doctorHint("Live health needs embedded CPA; runtime did not start")
		return
	}

	live := summarizeRuntimeAuthHealth(runtime, result)
	doctorKV("Source", "CPA runtime (coreManager.List)")
	doctorKV("Loaded", fmt.Sprintf("%d / %d configured", live.Loaded, result.ConfiguredMembers))
	doctorKV("Healthy", fmt.Sprintf("%d", live.Healthy))
	doctorKV("Invalid", fmt.Sprintf("%d", live.Invalid))
	doctorKV("Quota exhausted", fmt.Sprintf("%d", live.Quota))
	if live.Disabled > 0 {
		doctorKV("Disabled", fmt.Sprintf("%d", live.Disabled))
	}
	if live.Cooldown > 0 {
		doctorKV("Cooling down", fmt.Sprintf("%d", live.Cooldown))
	}
	printDoctorAuthMetadataCounts(live.Metadata)

	switch {
	case live.Healthy > 0 && live.Invalid == 0 && live.Quota == 0:
		doctorOK("Round-robin pool is healthy")
	case live.Healthy == 0:
		doctorWarn("No healthy credentials for round-robin")
		doctorHint("Accounts are marked in ~/.ccl/auth; CPA skips them until recovery or re-auth")
	default:
		doctorInfo(fmt.Sprintf("Pool degraded: %d usable of %d loaded", live.Healthy, live.Loaded))
	}
	if result.ConfiguredMembers > 0 && live.Loaded < result.ConfiguredMembers {
		doctorWarn(fmt.Sprintf("Only %d of %d group members loaded into CPA", live.Loaded, result.ConfiguredMembers))
	}
}

func printDoctorAuthMetadataCounts(counts authMetadataCounts) {
	fmt.Println(grayText.Render("  Metadata markers (persisted in credential JSON)"))
	doctorKV("unavailable", fmt.Sprintf("%d", counts.Unavailable))
	doctorKV("status", fmt.Sprintf("%d", counts.Status))
	doctorKV("status_message", fmt.Sprintf("%d", counts.StatusMessage))
	doctorKV("quota", fmt.Sprintf("%d", counts.Quota))
	doctorKV("next_retry_after", fmt.Sprintf("%d", counts.NextRetryAfter))
}

type authMetadataCounts struct {
	Unavailable    int
	Status         int
	StatusMessage  int
	Quota          int
	NextRetryAfter int
}

type runtimeAuthHealth struct {
	Loaded   int
	Healthy  int
	Invalid  int
	Quota    int
	Disabled int
	Cooldown int
	Metadata authMetadataCounts
}

func summarizeRuntimeAuthHealth(runtime *oauthproxy.Runtime, offline doctorGroupValidation) runtimeAuthHealth {
	var out runtimeAuthHealth
	if runtime == nil {
		return out
	}
	now := time.Now()
	for _, auth := range runtime.ListAuths() {
		if auth == nil {
			continue
		}
		out.Loaded++
		accumulateAuthMetadata(&out.Metadata, auth.Metadata, auth.Unavailable, string(auth.Status), auth.StatusMessage, auth.Quota.Exceeded, !auth.NextRetryAfter.IsZero())
		switch {
		case auth.Disabled || auth.Status == "disabled":
			out.Disabled++
			out.Invalid++
		case auth.Quota.Exceeded ||
			strings.Contains(strings.ToLower(auth.StatusMessage), "quota") ||
			strings.Contains(strings.ToLower(auth.Quota.Reason), "quota"):
			out.Quota++
		case auth.Unavailable || auth.Status == "error":
			out.Invalid++
		default:
			out.Healthy++
		}
		if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
			out.Cooldown++
		} else if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			out.Cooldown++
		}
	}
	// Members configured but missing from runtime still count as invalid relative to the group.
	if offline.ConfiguredMembers > out.Loaded {
		missing := offline.ConfiguredMembers - out.Loaded
		// Avoid double-counting offline invalid files already excluded from load.
		if offline.InvalidMembers < missing {
			out.Invalid += missing - offline.InvalidMembers
		}
	}
	return out
}

func countOfflineAuthMetadata(cfg *provider.Config, p provider.Provider) authMetadataCounts {
	var counts authMetadataCounts
	groupName := strings.TrimSpace(p.AuthGroup)
	if groupName == "" {
		return counts
	}
	group, ok := cfg.AuthGroups[groupName]
	if !ok {
		return counts
	}
	credentials, err := oauthproxy.ListCredentials()
	if err != nil {
		return counts
	}
	byFile := make(map[string]oauthproxy.CredentialInfo, len(credentials))
	for _, credential := range credentials {
		byFile[strings.ToLower(credential.FileName)] = credential
	}
	// ListCredentials already parses disabled/unavailable/quota, but not raw
	// status/status_message/next_retry_after presence. Re-read member files for
	// exact Metadata key counts that Save writes.
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		// Fall back to CredentialInfo flags only.
		for _, file := range group.Credentials {
			credential, exists := byFile[strings.ToLower(strings.TrimSpace(file))]
			if !exists {
				continue
			}
			if credential.Unavailable {
				counts.Unavailable++
			}
			if credential.QuotaExceeded {
				counts.Quota++
			}
			if credential.Status != "" {
				counts.Status++
			}
			if credential.StatusMessage != "" {
				counts.StatusMessage++
			}
		}
		return counts
	}
	for _, file := range group.Credentials {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(authDir, filepath.Base(file)))
		if err != nil {
			continue
		}
		metadata := map[string]any{}
		if json.Unmarshal(raw, &metadata) != nil {
			continue
		}
		accumulateAuthMetadata(&counts, metadata, false, "", "", false, false)
	}
	return counts
}

// accumulateAuthMetadata counts persisted health keys. When metadata is nil/partial,
// runtime field fallbacks still contribute so doctor reflects in-memory CPA state.
func accumulateAuthMetadata(counts *authMetadataCounts, metadata map[string]any, unavailable bool, status, statusMessage string, quotaExceeded, hasNextRetry bool) {
	if metadataHasKey(metadata, "unavailable") || unavailable {
		// Count only true unavailable, or key present with true; bare false should not inflate.
		if metadataBoolTrue(metadata, "unavailable") || (unavailable && !metadataHasKey(metadata, "unavailable")) {
			counts.Unavailable++
		}
	}
	if metadataStringPresent(metadata, "status") || strings.TrimSpace(status) != "" {
		if metadataStringPresent(metadata, "status") || strings.TrimSpace(status) != "" {
			// Prefer metadata presence; runtime status also counts when metadata lacks it.
			if metadataStringPresent(metadata, "status") || (!metadataHasKey(metadata, "status") && strings.TrimSpace(status) != "") {
				counts.Status++
			}
		}
	}
	if metadataStringPresent(metadata, "status_message") || strings.TrimSpace(statusMessage) != "" {
		if metadataStringPresent(metadata, "status_message") || (!metadataHasKey(metadata, "status_message") && strings.TrimSpace(statusMessage) != "") {
			counts.StatusMessage++
		}
	}
	if metadataHasKey(metadata, "quota") || quotaExceeded {
		if metadataHasKey(metadata, "quota") || (!metadataHasKey(metadata, "quota") && quotaExceeded) {
			counts.Quota++
		}
	}
	if metadataStringPresent(metadata, "next_retry_after") || hasNextRetry {
		if metadataStringPresent(metadata, "next_retry_after") || (!metadataHasKey(metadata, "next_retry_after") && hasNextRetry) {
			counts.NextRetryAfter++
		}
	}
}

func metadataHasKey(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	_, ok := metadata[key]
	return ok
}

func metadataBoolTrue(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	v, ok := metadata[key].(bool)
	return ok && v
}

func metadataStringPresent(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	v, ok := metadata[key].(string)
	return ok && strings.TrimSpace(v) != ""
}

func printDoctorProviderIdentity(p provider.Provider) {
	doctorKV("Kind", providerKindLabel(p))
	doctorKV("Protocol", provider.ProtocolLabel(p.Type))
	doctorKV("Auth", providerAuthLabel(p))
}

func printDoctorProviderDetails(p provider.Provider) {
	doctorKV("Endpoint", doctorEndpointDisplay(p))
	if p.AuthGroup == "" && p.OAuthProvider == "" {
		doctorKV("API Key", maskAPIKey(p.APIKey))
	} else if p.OAuthProvider != "" && p.AuthGroup == "" {
		cred := strings.TrimSpace(p.OAuthAccountCredential)
		if cred == "" {
			cred = "(all backend credentials)"
		}
		doctorKV("Credential", cred)
	}
	pool := parseModelList(p.Model)
	slotsConfigured := doctorConfiguredSlotCount(p)
	if len(pool) == 0 {
		// Model pool (provider.Model) is optional once Opus/Sonnet/... slots are set.
		// It only feeds map/set discovery, defaults, and bulk availability checks.
		if slotsConfigured > 0 {
			doctorKV("Model pool", "0 (optional)")
			doctorHint("Slot mappings below are what Claude Code uses; pool is only a candidate list for `ccl map`/`ccl set`")
		} else {
			doctorWarn("Model pool: 0 configured and no slot mappings")
			doctorHint("Run `ccl set` or `ccl map` to detect models and fill Opus/Sonnet/Haiku slots")
		}
	} else {
		doctorKV("Model pool", fmt.Sprintf("%d configured", len(pool)))
	}
	doctorKV("Effort", providerEffortSummary(p))
	doctorKV("Fast", providerFastSummary(p))
	doctorKV("Context/Compact", providerOneMSummary(p))
	doctorKV("Max Output", providerMaxOutputSummary(p))
	doctorKV("Tools", providerToolsSummary(p))
	doctorKV("Tool Search", providerToolSearchSummary(p))
	printProviderModelMappings(p)
}

func doctorConfiguredSlotCount(p provider.Provider) int {
	n := 0
	for _, model := range []string{p.OpusModel, p.SonnetModel, p.HaikuModel, p.CustomModelID, p.SubagentModel} {
		if strings.TrimSpace(model) != "" {
			n++
		}
	}
	return n
}

func doctorEndpointDisplay(p provider.Provider) string {
	ep := strings.TrimSpace(p.Endpoint)
	if ep == "" {
		return "(unset)"
	}
	return ep
}

func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(unset)"
	}
	runes := []rune(key)
	n := len(runes)
	if n <= 8 {
		return strings.Repeat("*", n)
	}
	mid := n - 8
	if mid > 12 {
		mid = 12
	}
	return string(runes[:4]) + strings.Repeat("*", mid) + string(runes[n-4:])
}

func providerMaxOutputSummary(p provider.Provider) string {
	runtimes := claude.ResolveRuntimeSettings(p)
	switch runtimes.MaxOutputTokens {
	case "", claude.DefaultMaxOutputTokens: // "32000"
		return "default (32K)"
	case "16000":
		return "16K"
	case "64000":
		return "64K"
	case "128000":
		return "128K"
	default:
		return runtimes.MaxOutputTokens
	}
}

func providerToolsSummary(p provider.Provider) string {
	runtimes := claude.ResolveRuntimeSettings(p)
	if runtimes.ToolUseConcurrency == "" || runtimes.ToolUseConcurrency == claude.DefaultToolUseConcurrency {
		return "default (3)"
	}
	return runtimes.ToolUseConcurrency
}

func providerToolSearchSummary(p provider.Provider) string {
	runtimes := claude.ResolveRuntimeSettings(p)
	switch strings.ToLower(strings.TrimSpace(runtimes.ToolSearch)) {
	case "", "false", "0", "off", "no":
		return "off"
	case "true", "1", "on", "yes":
		return "on"
	default:
		return runtimes.ToolSearch
	}
}

func printProviderModelMappings(p provider.Provider) {
	mappings := []struct {
		label string
		model string
	}{
		{"Opus", p.OpusModel},
		{"Sonnet", p.SonnetModel},
		{"Haiku", p.HaikuModel},
		{"Custom", p.CustomModelID},
		{"Subagent", subagentMappingDisplay(p)},
	}

	fmt.Println(grayText.Render("  Slot mappings"))
	for _, mapping := range mappings {
		model := mapping.model
		if model == "" {
			model = "(unset)"
		}
		doctorKV(mapping.label, model)
	}
}

// printModelReport displays the complete availability report for `ccl models`.
func printModelReport(available, unavailable []string) {
	if len(available) > 0 {
		fmt.Printf("Available (%d)\n", len(available))
		for _, m := range available {
			fmt.Printf("  ✓ %s\n", m)
		}
	}

	if len(unavailable) > 0 {
		if len(available) > 0 {
			fmt.Println()
		}
		fmt.Printf("Unavailable (%d)\n", len(unavailable))
		for _, m := range unavailable {
			fmt.Printf("  ✗ %s\n", m)
		}
	}

	fmt.Printf("\nSummary: %s\n", modelVerificationSummary(available, unavailable))
}

func RootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
