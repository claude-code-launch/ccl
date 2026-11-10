package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	probe := false
	target := &providerTarget{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show provider status and check connectivity",
		Long: `Show environment prerequisites, active provider status, and connectivity.

For API-key providers it prints the Review & Apply essentials (endpoint,
masked key, protocol, runtime, slot mappings). For subscription providers it
also summarizes credential health, including invalid and quota markers.

Model pool (provider.Model) is an optional candidate list used by ccl set/map
for discovery and bulk checks. Grok Build uses mapped default and helper models. An empty pool with filled slots is normal.

Doctor only reads: it lists models and checks the endpoint with a catalog
request, and never changes ~/.ccl/config.yaml. --probe additionally sends a
1-token request to every model in the pool, which counts against your plan.

Note: root "ccl status" is cloud sync status (see "ccl cloud status"), not this command.
For live request failures enable "ccl log on" and check the session log file
when the Grok Build session ends (default ~/.ccl/logs/ccl-debug-grok_<id>.log).
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), probe, *target)
		},
	}
	addProviderTargetFlags(cmd, target)
	cmd.Flags().BoolVar(&probe, "probe", false, "Also send a 1-token request to every model in the pool (billed)")
	return cmd
}

// ANSI 24-bit color helpers. doctor prints plain terminal output (no TUI), so
// it escapes directly instead of pulling in a styling library.
const ansiReset = "\x1b[0m"
const (
	ansiAccent    = "\x1b[38;2;101;183;255m" // #65B7FF
	ansiSecondary = "\x1b[38;2;183;156;255m" // #B79CFF
	ansiData      = "\x1b[38;2;65;215;200m"  // #41D7C8
	ansiWarn      = "\x1b[38;2;240;184;77m"  // #F0B84D
	ansiError     = "\x1b[38;2;255;138;128m" // #FF8A80
	ansiBold      = "\x1b[1m"
	ansiDim       = "\x1b[2m"
)

func doctorHeader(title string) {
	fmt.Println(ansiBold + ansiAccent + title + ansiReset)
	fmt.Println(ansiDim + strings.Repeat("─", 44) + ansiReset)
}

func doctorSection(title string) {
	fmt.Println()
	fmt.Println(ansiBold + ansiSecondary + "▸ " + title + ansiReset)
}

func doctorKV(label, value string) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "-"
	}
	fmt.Printf("  %s %s\n",
		ansiDim+fmt.Sprintf("%-18s", label)+ansiReset,
		ansiData+value+ansiReset,
	)
}

func doctorOK(msg string) {
	fmt.Printf("  %s %s\n", ansiData+ansiBold+"✓"+ansiReset, msg)
}

func doctorWarn(msg string) {
	fmt.Printf("  %s %s\n", ansiWarn+ansiBold+"!"+ansiReset, msg)
}

func doctorErr(msg string) {
	fmt.Printf("  %s %s\n", ansiError+"✗"+ansiReset, msg)
}

func doctorInfo(msg string) {
	fmt.Printf("  %s %s\n", ansiDim+"•"+ansiReset, msg)
}

func doctorHint(msg string) {
	fmt.Println(ansiDim + "  ↳ " + msg + ansiReset)
}

func runDoctor(ctx context.Context, probe bool, target providerTarget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	doctorHeader("ccl Doctor")

	doctorSection("Environment")

	if path, err := lookGrokBinary(); err == nil {
		doctorOK("Grok Build CLI: " + path)
	} else {
		doctorErr("Grok Build CLI: not installed")
		doctorHint("Install Grok Build from https://x.ai/cli")
	}

	cfg, err := config.Load()
	if err != nil {
		doctorErr(fmt.Sprintf("Config: %v", err))
		return nil
	}
	if provider.IsDefaultProvider(cfg.ActiveProvider) && target.name == "" {
		if strings.TrimSpace(cfg.LegacyClaudeDefault) != "" {
			doctorErr("The default Claude account has been retired. Run ccl use default or choose another provider.")
		} else {
			doctorInfo("Active provider: default (Grok Build's own login)")
		}
		printCloudSyncDiagnostics()
		return nil
	}
	doctorOK("Config: " + config.ConfigPath())
	printCloudSyncDiagnostics()

	// 3. Check the provider (active, or --provider)
	name, err := target.resolve(cfg)
	if err != nil {
		doctorSection("Provider")
		doctorErr(err.Error())
		return nil
	}
	doctorSection("Provider · " + name)
	p := cfg.Providers[name]

	printDoctorProviderIdentity(p)
	printDoctorProviderDetails(p)
	printProviderExperienceWarnings(p)

	configuredProvider := p
	p, runtime, cleanup, err := prepareProviderRuntime(context.Background(), p)
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

	// 4. Test Endpoint reachability and API Authentication key
	probeTarget := modelProbeTarget(configuredProvider, p)
	endpointReachable := probeTarget.Endpoint != "" && checkDoctorConnectivity(ctx, probeTarget)

	// 5. Optionally send a 1-token request to each pooled model. It costs real
	// requests (Qoder credits), so it only runs on
	// --probe, and doctor never rewrites the config with what it finds.
	if endpointReachable && p.Model != "" {
		configuredModels := parseModelList(p.Model)
		if len(configuredModels) > 0 {
			doctorSection("Model verification")
			if !probe {
				doctorInfo(fmt.Sprintf("Skipped: %d model(s) in the pool.", len(configuredModels)))
				doctorHint("Run `ccl doctor --probe` or `ccl models --probe` to send a 1-token test to each (billed).")
				return nil
			}
			availableSet := testModelsConcurrently(ctx, configuredModels, probeTarget.Endpoint, probeTarget.APIKey, probeWireType(probeTarget), probeTarget.ModelProtocols)
			available, unavailable := classifyModels(configuredModels, availableSet)
			doctorKV("Summary", modelVerificationSummary(available, unavailable))
			if len(unavailable) > 0 {
				doctorHint("Run `ccl models --probe` to inspect individual model results.")
			}
		}
	}

	return nil
}

// printDoctorContextBudget reports how this session will be sized.
//
// Codex advertises a default 200K or 1M window and two Balanced presets
// that compact at 85% of their advertised window.
//
// runtimeProvider carries the live endpoint/key of the embedded runtime;
// configured carries the user's ccl config.
func printDoctorContextBudget(runtimeProvider, configured provider.Provider, modelNames map[string]string) {
	doctorSection("Context budget")

	// The effective context env is the preset's expansion, or hand-written
	// values when there is no preset.
	contextEnv := provider.ContextPresetEnv(configured)
	if contextEnv == nil {
		contextEnv = configured.Env
	}
	maxContext := parseDoctorTokenEnv(contextEnv[provider.EnvMaxContextTokens])
	compactWindow := parseDoctorTokenEnv(contextEnv[provider.EnvAutoCompactWindow])
	compactPct := strings.TrimSpace(contextEnv[provider.EnvAutoCompactPct])
	overridden := maxContext > 0 || compactWindow > 0 || compactPct != ""
	// Never probe with an unset endpoint: NormalizeOpenAIModelsURL falls back to
	// api.openai.com, which would ship this provider's key to OpenAI. Fixed
	// managed catalogs such as AutoClaw do not serve this generic catalog either.
	var windows map[string]int
	var source string
	if endpoint := strings.TrimSpace(runtimeProvider.Endpoint); endpoint != "" &&
		!provider.IsAnthropicType(runtimeProvider.Type) {
		windows, source = advertisedContextWindows(endpoint, runtimeProvider.APIKey)
	}
	smallest, smallestModel, unknown := smallestMappedWindow(configured, windows)

	oauthproxy.LogDebugf("doctor context budget provider=%q max_context=%d compact_window=%d compact_pct=%q balanced=%t catalog=%q models=%d smallest=%d smallest_model=%q",
		configured.Name, maxContext, compactWindow, compactPct,
		provider.ProviderContextPreset(configured) != provider.ContextPresetDefault, source, len(windows), smallest, smallestModel)

	oneMSlots := oneMSlotsFromProvider(configured)
	preset := provider.ProviderContextPreset(configured)
	balanced := preset != provider.ContextPresetDefault
	unsupported := overridden && !balanced
	balancedWindow := ""
	balancedCompactAt := ""
	switch preset {
	case provider.ContextPresetBalanced500K:
		balancedWindow = "500K"
		balancedCompactAt = "425K"
	case provider.ContextPresetBalanced800K:
		balancedWindow = "800K"
		balancedCompactAt = "680K"
	}
	if balanced {
		doctorKV("Sizing", fmt.Sprintf("Balanced %s / %s", balancedWindow, balancedCompactAt))
		doctorKV("Assumed context", fmt.Sprintf("%s (%d)", balancedWindow, maxContext))
		doctorKV("Auto-compact window", fmt.Sprintf("%s (%d)", balancedWindow, compactWindow))
		doctorKV("Auto-compact pct", fmt.Sprintf("85%% (~%s)", balancedCompactAt))
	} else if unsupported {
		doctorKV("Sizing", "Default (unsupported context override is ignored)")
		doctorKV("Auto-compact at", "Codex default for the slot's window")
		overridden = false
	} else {
		doctorKV("Sizing", "Default (Codex 200K / 1M)")
		doctorKV("Auto-compact at", "Codex default for the slot's window")
	}

	// Default sizes per slot; either Balanced tier intentionally applies one
	// provider-wide cap.
	for _, slot := range provider.SlotModels(configured) {
		sizing := balancedWindow + " Balanced"
		if !balanced {
			sizing = "200K default"
		}
		if !balanced && oneMSlots[slot.Slot] {
			sizing = "1M ([1m])"
		}
		advertised := "backend window unknown"
		if window, ok := windows[strings.ToLower(slot.Model)]; ok && window > 0 {
			advertised = "backend " + formatTokenCount(window) + ", " + contextClassLabel(window)
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
	if mappedContextClassesDiffer(configured, windows) {
		if balanced {
			doctorInfo(fmt.Sprintf("Mapped models span both context classes; Default sizes them per slot, while Balanced applies one %s cap", balancedWindow))
		} else {
			doctorInfo("Mapped models span both context classes; Default sizes them independently per slot")
		}
	}
	printDoctorIgnoredContextEnv()
	if !overridden {
		doctorOK("Default mode; Codex keeps its native 200K/1M sizing")
		return
	}
	if smallest > 0 && maxContext > smallest {
		doctorWarn(fmt.Sprintf("Balanced context %s exceeds the %s window of %s",
			formatTokenCount(maxContext), providerCatalogModelLabel(smallestModel, modelNames), formatTokenCount(smallest)))
		doctorHint("Requests can be rejected with context_length_exceeded (HTTP 400) before Codex auto-compacts")
	}
	doctorOK(fmt.Sprintf("Balanced mode; Codex compacts at 85%% of the %s window (~%s)", balancedWindow, balancedCompactAt))
}

// printDoctorOneMConsistency checks the [1m] markers against the advertised
// windows.
//
// The [1m] suffix tells Codex to use a 1M window and its matching compact limit.
func printDoctorOneMConsistency(p provider.Provider, windows map[string]int) {
	if len(windows) == 0 || provider.ProviderContextPreset(p) != provider.ContextPresetDefault {
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
		doctorHint("Codex sizes the session (and its compact buffer) for 1M, so it compacts after the backend already refuses the request")
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
			doctorInfo(fmt.Sprintf("%s=%s is set in your shell and is ignored: ccl passes only its own variables to Codex", key, value))
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

// doctorRedactBody keeps an upstream diagnostic bounded and avoids echoing the
// configured API key if a gateway includes it in an error body.
func doctorRedactBody(body string, secrets ...string) string {
	body = strings.TrimSpace(body)
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			body = strings.ReplaceAll(body, secret, "[redacted]")
		}
	}
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return body
}

// checkDoctorConnectivity probes the provider endpoint and reports whether it is
// reachable and authenticated. It lives in its own function so every response
// body and context is released here instead of at the end of the whole run.
func checkDoctorConnectivity(ctx context.Context, p provider.Provider) bool {
	doctorSection("Connectivity")
	doctorInfo("Checking endpoint and credentials...")
	client := &http.Client{Timeout: doctorProbeTimeout}

	modelsURL := protocol.NormalizeOpenAIModelsURL(p.Endpoint)

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

// modelProbeTarget picks the provider a model-verification probe should hit.
// API-key providers are probed on their configured upstream. OAuth providers
// use the local Responses adapter with its bound credential.
func modelProbeTarget(configured, runtime provider.Provider) provider.Provider {
	if strings.TrimSpace(configured.OAuthProvider) != "" {
		return runtime
	}
	return configured
}

// testModelsConcurrently tests multiple models in small concurrent batches.
// Each worker sends a lightweight provider-specific POST to verify the model
// works. protocols carries the per-model wire table for mixed-protocol
// models.dev gateways (nil for single-protocol providers).
// Returns a set of model IDs that passed the test.
func testModelsConcurrently(ctx context.Context, models []string, endpoint, apiKey, providerType string, protocols map[string]string) map[string]bool {
	if ctx == nil {
		ctx = context.Background()
	}
	// 8 workers, not 50: a large concurrent burst against one gateway trips
	// per-key rate limits and marks healthy models unavailable. doctor is a
	// deliberate one-shot diagnosis, so it batches wider than the ccl set page's
	// interactive slotTestConcurrency (2), which re-runs probes while typing.
	const batchSize = 8

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
				ok := testSingleModelWithProtocolsContext(ctx, m, endpoint, apiKey, providerType, protocols, modelProbeTimeout)
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

// testSingleModelWithProtocolsContext probes one model using a per-model
// protocol table. Mixed-protocol models.dev gateways expose chat, Responses
// and native-Anthropic models behind one endpoint, so the probe for each model
// must follow that model's declared wire protocol — probing them all as Chat
// Completions marks working models unavailable.
func testSingleModelWithProtocolsContext(ctx context.Context, model, endpoint, apiKey, providerType string, protocols map[string]string, timeout time.Duration) bool {
	status, err := probeSingleModelWithProtocolsStatusContext(ctx, model, endpoint, apiKey, providerType, protocols, timeout)
	return err == nil && status >= 200 && status < 300
}

func probeSingleModelWithProtocolsStatusContext(ctx context.Context, model, endpoint, apiKey, providerType string, protocols map[string]string, timeout time.Duration) (int, error) {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	wire := "openai"
	switch {
	case provider.IsAutoClawType(providerType):
		wire = "openai_responses"
	case provider.IsAnthropicType(providerType):
		wire = "anthropic"
	case provider.IsOpenAIResponsesType(providerType):
		wire = "openai_responses"
	case provider.IsModelsDevType(providerType):
		wire = probeProtocolForModel(protocols, model)
	}
	return probeSingleModelForProtocolStatusContext(ctx, model, endpoint, apiKey, wire, timeout)
}

// probeProtocolForModel resolves the wire protocol a mixed-protocol models.dev
// gateway uses for one model; unknown models fall back to chat, mirroring the
// runtime's mixedProtocolForModel fallback.
func probeProtocolForModel(protocols map[string]string, model string) string {
	for _, id := range []string{strings.TrimSpace(model), stripOneMSuffix(model)} {
		if proto := protocols[strings.ToLower(id)]; proto != "" {
			return proto
		}
	}
	return "openai"
}

func probeSingleModelForProtocolStatusContext(ctx context.Context, model, endpoint, apiKey, wireProtocol string, timeout time.Duration) (int, error) {
	model = stripOneMSuffix(model)
	switch wireProtocol {
	case "anthropic":
		return 0, fmt.Errorf("Anthropic Messages has been retired")
	case "openai_responses":
		return protocol.ProbeOpenAIResponsesStatusContext(ctx, endpoint, apiKey, model, timeout)
	default:
		return probeSingleOpenAIModelStatusContext(ctx, model, endpoint, apiKey, timeout)
	}
}

// probeModelStatus reports the upstream HTTP status code (0 on transport
// error). The models.dev key verifier relies on
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

func probeSingleOpenAIModelStatusContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) (int, error) {
	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	// The token limit exists only to keep the probe cheap; an empty tokenField
	// omits it entirely.
	payload := func(tokenField string) map[string]any {
		body := map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "Reply OK."}},
			"stream":   false,
		}
		if tokenField != "" {
			body[tokenField] = 1
		}
		return body
	}
	// ClinePass documents a Chat Completions request with the full model slug,
	// messages, and stream:false, without a token-limit field. Sending
	// max_tokens:1 can reject otherwise usable models on this gateway.
	if strings.HasPrefix(strings.ToLower(model), "cline-pass/") {
		return probeModelStatus(parent, buildChatURL(endpoint), payload(""), headers, timeout)
	}
	status, err := probeModelStatus(parent, buildChatURL(endpoint), payload("max_tokens"), headers, timeout)
	if err != nil || status == http.StatusUnauthorized || status == http.StatusForbidden || status >= 200 && status < 300 {
		return status, err
	}
	// Reasoning-model families renamed max_tokens to max_completion_tokens and
	// reject the old parameter with a 400; retry once before declaring the
	// model unavailable.
	if status == http.StatusBadRequest {
		status, err = probeModelStatus(parent, buildChatURL(endpoint), payload("max_completion_tokens"), headers, timeout)
		if err != nil || status == http.StatusUnauthorized || status == http.StatusForbidden || status >= 200 && status < 300 {
			return status, err
		}
	}
	if status != http.StatusBadRequest && status < 500 {
		return status, nil
	}
	// A gateway that fans each model out to a different backend can fail a
	// one-token probe on a backend that produces no content within the limit
	// (ClinePass answers 500 "empty response content" for several of them).
	// Ask once more with no token limit before reporting the model — and, for
	// key verification, the whole provider — as unusable.
	return probeModelStatus(parent, buildChatURL(endpoint), payload(""), headers, timeout)
}

// buildChatURL constructs a chat completions endpoint URL from a provider endpoint.
func buildChatURL(endpoint string) string {
	return protocol.NormalizeOpenAIChatCompletionsURL(endpoint)
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
	fmt.Println(ansiDim + "  Metadata markers (persisted in credential JSON)" + ansiReset)
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
// runtime field fallbacks still contribute so doctor reflects in-memory runtime state.
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
			doctorHint("Mapped models can be used even when the pool is empty")
		} else {
			doctorWarn("Model pool: 0 configured and no slot mappings")
			doctorHint("Run `ccl set` or `ccl map` to detect models and fill the model slots")
		}
	} else {
		doctorKV("Model pool", fmt.Sprintf("%d configured", len(pool)))
	}
	doctorKV("Effort", providerEffortSummary(p))
	doctorKV("Fast", providerFastSummary(p))
	doctorKV("Context/Compact", providerOneMSummary(p))
}

func doctorConfiguredSlotCount(p provider.Provider) int {
	n := 0
	for _, model := range []string{p.OpusModel, p.SonnetModel, p.HaikuModel, p.FableModel, p.CustomModelID, p.SubagentModel} {
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

func printProviderModelMappings(p provider.Provider, modelNames map[string]string) {
	mappings := []struct {
		label string
		model string
	}{
		{"Opus", providerCatalogModelLabel(p.OpusModel, modelNames)},
		{"Sonnet", providerCatalogModelLabel(p.SonnetModel, modelNames)},
		{"Haiku", providerCatalogModelLabel(p.HaikuModel, modelNames)},
		{"Fable", providerCatalogModelLabel(p.FableModel, modelNames)},
		{"Custom", providerCatalogModelLabel(p.CustomModelID, modelNames)},
		{"Subagent", subagentMappingDisplayWithNames(p, modelNames)},
	}

	fmt.Println(ansiDim + "  Slot mappings" + ansiReset)
	for _, mapping := range mappings {
		model := mapping.model
		if model == "" {
			model = "(unset)"
		}
		doctorKV(mapping.label, model)
	}
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
	label := id
	if displayName := strings.TrimSpace(info.DisplayName); displayName != "" {
		label = displayName
		if !strings.EqualFold(displayName, id) {
			label += " (" + id + ")"
		}
	}
	return appendModelBadges(label, info)
}

// modelBadgeLabel renders a model as its ID plus the catalog's cost and
// freshness markers, without the display alias modelReportLabel prefixes. The
// interactive picker uses it so a row reads as the ID being chosen while the
// pricing signals that inform the choice stay visible.
func modelBadgeLabel(id string, metadata map[string]protocol.ModelInfo) string {
	info, ok := metadata[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return id
	}
	return appendModelBadges(id, info)
}

func appendModelBadges(label string, info protocol.ModelInfo) string {
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
