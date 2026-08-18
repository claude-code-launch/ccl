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

For API-key providers it prints the Review & Apply essentials (endpoint,
masked key, protocol, runtime, slot mappings). For subscription providers it
also summarizes credential health, including invalid and quota markers.

Model pool (provider.Model) is an optional candidate list used by ccl set/map
for discovery and bulk checks. Claude Code actually uses the slot mappings
(Opus/Sonnet/Haiku/Custom/Subagent). An empty pool with filled slots is normal.

Note: root "ccl status" is cloud sync status (see "ccl cloud status"), not this command.
For live request failures enable "ccl log on" and check the session log file
when the Claude session ends (default ~/.ccl/logs/ccl-debug-claude_<id>.log).
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
		// Report it, do not fix it: doctor is a diagnostic, so it must not
		// download and run an installer as a side effect of being asked for a
		// health report. `ccl install` and launching a session both offer that.
		doctorErr("Claude Code CLI: not installed or not in PATH")
		doctorHint("Run `ccl` to be offered the installer, or install manually: https://code.claude.com/")
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
	printDoctorProviderDetails(p)
	printProviderExperienceWarnings(p)

	configuredProvider := p
	p, runtime, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		printProviderModelMappings(configuredProvider, nil)
		if configuredProvider.OAuthProvider != "" {
			printDoctorOAuthHealth(configuredProvider, nil)
		}
		doctorSection("Connectivity")
		doctorErr(err.Error())
		return nil
	}
	defer cleanup()
	if configuredProvider.OAuthProvider != "" {
		printDoctorOAuthHealth(configuredProvider, runtime)
	}
	modelNames := runtime.ModelDisplayNames()
	printProviderModelMappings(configuredProvider, modelNames)
	printDoctorContextBudget(p, configuredProvider, modelNames)

	// 5. Test Endpoint reachability and API Authentication key
	if p.Endpoint != "" {
		endpointReachable := checkDoctorConnectivity(ctx, p)

		// 6. Validate configured models with concurrent API calls and reorder (available first)
		if endpointReachable && p.Model != "" {
			configuredModels := parseModelList(p.Model)
			if len(configuredModels) > 0 {
				doctorSection("Model verification")
				availableSet := testModelsConcurrently(ctx, configuredModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth, p.ModelProtocols)
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

// printDoctorContextBudget reports how this session will be sized.
//
// Default leaves Claude Code on its native 200K/1M per-slot behavior. Balanced
// declares a 500K window and compacts at 80% (approximately 400K). This section
// shows the effective mode and checks it against advertised backend windows.
//
// runtimeProvider carries the live endpoint/key of the embedded runtime;
// configured carries the user's ccl config.
func printDoctorContextBudget(runtimeProvider, configured provider.Provider, modelNames map[string]string) {
	doctorSection("Context budget")

	maxContext := parseDoctorTokenEnv(configured.Env[maxContextTokensEnv])
	compactWindow := parseDoctorTokenEnv(configured.Env[autoCompactWindowEnv])
	compactPct := strings.TrimSpace(configured.Env[provider.EnvAutoCompactPct])
	overridden := maxContext > 0 || compactWindow > 0 || compactPct != ""
	// Never probe with an unset endpoint: NormalizeOpenAIModelsURL falls back to
	// api.openai.com, which would ship this provider's key to OpenAI. Anthropic
	// gateways do not serve this catalog either.
	var windows map[string]int
	var source string
	if endpoint := strings.TrimSpace(runtimeProvider.Endpoint); endpoint != "" && !provider.IsAnthropicType(runtimeProvider.Type) {
		windows, source = claude.AdvertisedContextWindows(endpoint, runtimeProvider.APIKey)
	}
	smallest, smallestModel, unknown := smallestMappedWindow(configured, windows)

	oauthproxy.LogDebugf("doctor context budget provider=%q max_context=%d compact_window=%d compact_pct=%q balanced=%t catalog=%q models=%d smallest=%d smallest_model=%q",
		configured.Name, maxContext, compactWindow, compactPct,
		provider.IsBalancedContextPreset(configured.Env), source, len(windows), smallest, smallestModel)

	oneMSlots := oneMSlotsFromProvider(configured)
	balanced := provider.IsBalancedContextPreset(configured.Env)
	unsupported := overridden && !balanced
	if balanced {
		doctorKV("Sizing", "Balanced 500K / 400K")
		doctorKV("Assumed context", "500K (500000)")
		doctorKV("Auto-compact window", "500K (500000)")
		doctorKV("Auto-compact pct", "80% (~400K)")
	} else if unsupported {
		doctorKV("Sizing", "Default (unsupported context override is ignored)")
		doctorKV("Auto-compact at", "Claude Code default for the slot's window")
		overridden = false
	} else {
		doctorKV("Sizing", "Default (Claude Code 200K / 1M)")
		doctorKV("Auto-compact at", "Claude Code default for the slot's window")
	}

	// Default sizes per slot; Balanced intentionally applies one global 500K cap.
	for _, slot := range provider.SlotModels(configured) {
		sizing := "500K Balanced"
		if !balanced {
			sizing = "200K default"
		}
		if !balanced && oneMSlots[slot.Slot] {
			sizing = "1M ([1m])"
		}
		advertised := "backend window unknown"
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			advertised = "backend " + formatTokenCount(window) + ", " + claude.ContextClassLabel(window)
		}
		doctorKV(slot.Slot, fmt.Sprintf("%s · %s · %s", providerCatalogModelLabel(slot.Model, modelNames), sizing, advertised))
	}
	if len(windows) == 0 {
		doctorInfo("Backend does not advertise context windows, so [1m] markers cannot be verified here")
		return
	}
	doctorKV("Window source", source)
	if unknown > 0 {
		doctorInfo(fmt.Sprintf("%d mapped model(s) are absent from the catalog; their window is unknown", unknown))
	}

	printDoctorOneMConsistency(configured, windows)
	if claude.MappedContextClassesDiffer(configured, windows) {
		doctorInfo("Mapped models span both context classes; Default sizes them per slot, while Balanced applies one 500K cap")
	}
	printDoctorIgnoredContextEnv()
	if !overridden {
		doctorOK("Default mode; Claude Code keeps its native 200K/1M sizing")
		return
	}
	if smallest > 0 && maxContext > smallest {
		doctorWarn(fmt.Sprintf("Balanced context %s exceeds the %s window of %s",
			formatTokenCount(maxContext), providerCatalogModelLabel(smallestModel, modelNames), formatTokenCount(smallest)))
		doctorHint("Requests can be rejected with context_length_exceeded (HTTP 400) before Claude Code auto-compacts")
	}
	doctorOK("Balanced mode; Claude Code compacts at 80% of the 500K window (~400K)")
}

// printDoctorOneMConsistency checks the [1m] markers against the advertised
// windows.
//
// In Default mode the suffix tells Claude Code a slot runs the 1M variant: it
// sizes the session for a 1M window and scales its auto-compact buffer to it, so
// a [1m] marker on a model whose backend window is far smaller makes Claude Code
// compact much too late and the upstream rejects the turn first. The suffix is
// only an Anthropic capability signal — it never widens a third-party window.
func printDoctorOneMConsistency(p provider.Provider, windows map[string]int) {
	if len(windows) == 0 || provider.IsBalancedContextPreset(p.Env) {
		return
	}
	oneMSlots := oneMSlotsFromProvider(p)
	if len(oneMSlots) == 0 {
		return
	}
	for _, slot := range provider.SlotModels(p) {
		if !oneMSlots[slot.Slot] {
			continue
		}
		window, ok := windows[strings.ToLower(slot.Model)]
		if !ok || window <= 0 || protocol.ContextWindowSuggests1M(window) {
			continue
		}
		doctorWarn(fmt.Sprintf("%s is marked [1m] but %s advertises only %s",
			slot.Slot, slot.Model, formatTokenCount(window)))
		doctorHint("Claude Code sizes the session (and its compact buffer) for 1M, so it compacts after the backend already refuses the request")
		doctorHint("Clear the Extended Context checkbox for that slot in `ccl set`, or confirm the backend really accepts 1M")
	}
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

// printDoctorIgnoredContextEnv names context variables exported in the shell.
//
// ccl no longer passes those through, so a value left over from an older
// workaround has no effect. Saying so is the difference between "my export does
// nothing" being a mystery and being expected.
func printDoctorIgnoredContextEnv() {
	for _, key := range provider.ManagedContextEnvKeys() {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			doctorInfo(fmt.Sprintf("%s=%s is set in your shell and is ignored: ccl passes only its own variables to Claude Code", key, value))
		}
	}
}

func parseDoctorTokenEnv(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
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

// testModelsConcurrently tests multiple models in small concurrent batches.
// Each worker sends a lightweight provider-specific POST to verify the model
// works. protocols carries the per-model wire table for mixed-protocol
// models.dev gateways (nil for single-protocol providers).
// Returns a set of model IDs that passed the test.
func testModelsConcurrently(ctx context.Context, models []string, endpoint, apiKey, providerType, anthropicAuth string, protocols map[string]string) map[string]bool {
	if ctx == nil {
		ctx = context.Background()
	}
	// 8 workers, not 50: a large concurrent burst against one gateway trips
	// per-key rate limits and marks healthy models unavailable.
	const batchSize = 8
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
				ok := testSingleModelWithProtocolsContext(ctx, m, endpoint, apiKey, providerType, anthropicAuth, protocols, requestTimeout)
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
	return testSingleModelWithProtocolsContext(ctx, model, endpoint, apiKey, providerType, anthropicAuth, nil, timeout)
}

// testSingleModelWithProtocolsContext is testSingleModelContext with a
// per-model protocol table. Mixed-protocol models.dev gateways expose chat,
// Responses and native-Anthropic models behind one endpoint, so the probe for
// each model must follow that model's declared wire protocol — probing them
// all as Chat Completions marks working models unavailable.
func testSingleModelWithProtocolsContext(ctx context.Context, model, endpoint, apiKey, providerType, anthropicAuth string, protocols map[string]string, timeout time.Duration) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	wire := "openai"
	switch {
	case provider.IsAnthropicType(providerType):
		wire = "anthropic"
	case provider.IsOpenAIResponsesType(providerType):
		wire = "openai_responses"
	case provider.IsModelsDevType(providerType):
		wire = probeProtocolForModel(protocols, model)
	}
	return testSingleModelForProtocolContext(ctx, model, endpoint, apiKey, wire, anthropicAuth, timeout)
}

// probeProtocolForModel resolves the wire protocol a mixed-protocol models.dev
// gateway uses for one model; unknown models fall back to chat, mirroring the
// runtime's mixedProtocolForModel fallback.
func probeProtocolForModel(protocols map[string]string, model string) string {
	if proto, ok := protocols[strings.ToLower(strings.TrimSpace(model))]; ok && proto != "" {
		return proto
	}
	return "openai"
}

// testSingleModelForProtocolContext probes one model over an explicit wire
// protocol: "anthropic", "openai_responses", or Chat Completions for anything
// else. A trailing [1m] context marker is stripped first — it is a ccl slot
// directive, not part of the upstream model name.
func testSingleModelForProtocolContext(ctx context.Context, model, endpoint, apiKey, wireProtocol, anthropicAuth string, timeout time.Duration) bool {
	model = stripOneMSuffix(model)
	switch wireProtocol {
	case "anthropic":
		if strings.TrimSpace(anthropicAuth) == "" {
			anthropicAuth = "x-api-key"
		}
		return testSingleAnthropicModelWithAuthContext(ctx, model, endpoint, apiKey, anthropicAuth, timeout)
	case "openai_responses":
		return testSingleOpenAIResponsesModelContext(ctx, model, endpoint, apiKey, timeout)
	default:
		return testSingleOpenAIModelContext(ctx, model, endpoint, apiKey, timeout)
	}
}

// probeModel sends one minimal completion request and reports whether the
// upstream accepted it. The OpenAI and Anthropic probes differ only in URL,
// payload and auth headers.
func probeModel(parent context.Context, url string, payload map[string]any, headers map[string]string, timeout time.Duration) bool {
	status, err := probeModelStatus(parent, url, payload, headers, timeout)
	if err != nil {
		return false
	}
	return status >= 200 && status < 300
}

// probeModelStatus is probeModel but reports the upstream HTTP status code (0 on
// transport error) instead of a boolean. The models.dev key verifier relies on
// the distinction between an auth rejection (401/403) and a valid key that hits a
// model/parameter/rate-limit problem (any other 4xx).
func probeModelStatus(parent context.Context, url string, payload map[string]any, headers map[string]string, timeout time.Duration) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func testSingleOpenAIModelContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	payload := func(tokenField string) map[string]any {
		return map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
			tokenField: 1,
		}
	}
	status, err := probeModelStatus(parent, buildChatURL(endpoint), payload("max_tokens"), headers, timeout)
	if err != nil {
		return false
	}
	if status >= 200 && status < 300 {
		return true
	}
	// Reasoning-model families renamed max_tokens to max_completion_tokens and
	// reject the old parameter with a 400; retry once before declaring the
	// model unavailable.
	if status == http.StatusBadRequest {
		status, err = probeModelStatus(parent, buildChatURL(endpoint), payload("max_completion_tokens"), headers, timeout)
		if err != nil {
			return false
		}
	}
	return status >= 200 && status < 300
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
// preserving original relative order in both results.
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

type doctorOAuthSnapshot struct {
	Configured int
	Present    int
	Healthy    int
	Invalid    int
	Quota      int
	Disabled   int
	Files      []string
}

func printDoctorOAuthHealth(p provider.Provider, runtime *oauthproxy.Runtime) {
	doctorSection("Subscription account")
	if runtime == nil {
		offline := inspectDoctorOAuthCredential(p)
		doctorKV("Source", "offline files (~/.ccl/auth)")
		doctorKV("Configured", fmt.Sprintf("%d", offline.Configured))
		doctorKV("Present", fmt.Sprintf("%d", offline.Present))
		doctorKV("Healthy", fmt.Sprintf("%d", offline.Healthy))
		doctorKV("Invalid", fmt.Sprintf("%d", offline.Invalid))
		doctorKV("Quota flagged", fmt.Sprintf("%d", offline.Quota))
		if offline.Disabled > 0 {
			doctorKV("Disabled", fmt.Sprintf("%d", offline.Disabled))
		}
		printDoctorAuthMetadataCounts(countOfflineAuthMetadata(offline.Files))
		doctorHint("Live health needs the embedded subscription runtime")
		return
	}

	live := summarizeRuntimeAuthHealth(runtime)
	doctorKV("Source", "subscription runtime")
	doctorKV("Loaded", fmt.Sprintf("%d", live.Loaded))
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
		doctorOK("Subscription credential is healthy")
	case live.Healthy == 0:
		doctorWarn("No healthy subscription credential loaded")
		doctorHint("Authenticate the subscription account again with `ccl oauth <provider> [alias]`")
	default:
		doctorInfo(fmt.Sprintf("Credential health degraded: %d usable of %d loaded", live.Healthy, live.Loaded))
	}
}

func inspectDoctorOAuthCredential(p provider.Provider) doctorOAuthSnapshot {
	var out doctorOAuthSnapshot
	target := strings.TrimSpace(p.OAuthAccountCredential)
	if target != "" {
		out.Configured = 1
	}
	backend, err := oauthproxy.BackendProvider(p.OAuthProvider)
	if err != nil {
		out.Invalid = out.Configured
		return out
	}
	credentials, err := oauthproxy.ListCredentials()
	if err != nil {
		out.Invalid = out.Configured
		return out
	}
	for _, credential := range credentials {
		if !strings.EqualFold(credential.Backend, backend) {
			continue
		}
		if target != "" && !strings.EqualFold(credential.FileName, target) {
			continue
		}
		out.Present++
		out.Files = append(out.Files, credential.FileName)
		switch {
		case credential.Disabled:
			out.Disabled++
			out.Invalid++
		case credential.QuotaExceeded:
			out.Quota++
		case credential.Unavailable:
			out.Invalid++
		default:
			out.Healthy++
		}
	}
	if target == "" {
		out.Configured = out.Present
	} else if out.Present == 0 {
		out.Invalid++
	}
	return out
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

func summarizeRuntimeAuthHealth(runtime *oauthproxy.Runtime) runtimeAuthHealth {
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
	return out
}

func countOfflineAuthMetadata(files []string) authMetadataCounts {
	var counts authMetadataCounts
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		return counts
	}
	for _, file := range files {
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
	doctorKV("Protocol", provider.ProtocolLabelForProvider(p))
	doctorKV("Auth", providerAuthLabel(p))
}

func printDoctorProviderDetails(p provider.Provider) {
	doctorKV("Endpoint", doctorEndpointDisplay(p))
	if p.OAuthProvider == "" {
		doctorKV("API Key", maskAPIKey(p.APIKey))
	} else {
		cred := strings.TrimSpace(p.OAuthAccountCredential)
		if cred == "" {
			cred = "(not bound; authenticate this subscription again)"
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
	doctorKV("Tools", providerToolsSummary(p))
	doctorKV("Tool Search", providerToolSearchSummary(p))
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

func printProviderModelMappings(p provider.Provider, modelNames map[string]string) {
	mappings := []struct {
		label string
		model string
	}{
		{"Opus", providerCatalogModelLabel(p.OpusModel, modelNames)},
		{"Sonnet", providerCatalogModelLabel(p.SonnetModel, modelNames)},
		{"Haiku", providerCatalogModelLabel(p.HaikuModel, modelNames)},
		{"Custom", providerCatalogModelLabel(p.CustomModelID, modelNames)},
		{"Subagent", subagentMappingDisplayWithNames(p, modelNames)},
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
	printModelReportWithMetadata(available, unavailable, nil)
}

func printModelReportWithMetadata(available, unavailable []string, metadata map[string]protocol.ModelInfo) {
	if len(available) > 0 {
		fmt.Printf("Available (%d)\n", len(available))
		for _, m := range available {
			fmt.Printf("  ✓ %s\n", modelReportLabel(m, metadata))
		}
	}

	if len(unavailable) > 0 {
		if len(available) > 0 {
			fmt.Println()
		}
		fmt.Printf("Unavailable (%d)\n", len(unavailable))
		for _, m := range unavailable {
			fmt.Printf("  ✗ %s\n", modelReportLabel(m, metadata))
		}
	}

	fmt.Printf("\nSummary: %s\n", modelVerificationSummary(available, unavailable))
}

func modelReportLabel(id string, metadata map[string]protocol.ModelInfo) string {
	info, ok := metadata[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return id
	}
	displayName := strings.TrimSpace(info.DisplayName)
	label := id
	if displayName != "" {
		label = displayName
		if !strings.EqualFold(displayName, id) {
			label += " (" + id + ")"
		}
	}
	if info.RateMultiplier != nil {
		label += " · " + strconv.FormatFloat(*info.RateMultiplier, 'f', -1, 64) + "x"
	}
	if info.IsNew {
		label += " · new"
	}
	if info.PromotionAvailable {
		label += " · off-peak discount"
	}
	return label
}

func RootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
