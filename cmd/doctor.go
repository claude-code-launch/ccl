package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

var doctorCmd = newDoctorCommand("doctor")

func newDoctorCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Check system prerequisites and provider connectivity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor()
		},
	}
}

func runDoctor() error {
	fmt.Println("ccl Doctor")
	fmt.Println("==========")
	fmt.Println("Environment")

	// 1. Check Node.js
	nodePath, err := exec.LookPath("node")
	if err != nil {
		fmt.Println("  • Node.js: not installed (not required by newer native Claude Code binaries)")
	} else {
		fmt.Printf("  ✓ Node.js: %s\n", nodePath)
	}

	// 2. Check Claude CLI
	claudeInstalled := IsInstalled()
	if !claudeInstalled {
		fmt.Println("  ✗ Claude Code CLI: not installed or not in PATH")
		// Prompt to install automatically
		err := AutoInstall()
		if err != nil {
			fmt.Printf("  ✗ Auto-installation failed: %v\n", err)
			fmt.Println("    Install manually: https://code.claude.com/")
		}
	} else {
		claudePath, _ := exec.LookPath("claude")
		fmt.Printf("  ✓ Claude Code CLI: %s\n", claudePath)
	}

	// 3. Check Configuration File
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("  ✗ Config: %v\n", err)
		return nil
	}
	fmt.Printf("  ✓ Config: %s\n", config.ConfigPath())

	// 4. Check Active Provider
	if cfg.ActiveProvider == "" {
		fmt.Println("\nProvider\n  ✗ No active provider. Use `ccl set` or `ccl use`.")
		return nil
	}
	fmt.Printf("\nProvider · %s\n", cfg.ActiveProvider)

	p, ok := cfg.Providers[cfg.ActiveProvider]
	if !ok {
		fmt.Printf("  ✗ Selected provider %q does not exist in config\n", cfg.ActiveProvider)
		return nil
	}

	fmt.Printf("  Protocol: %s\n", provider.ProtocolLabel(p.Type))
	fmt.Printf("  Auth: %s\n", providerAuthLabel(p))
	fmt.Printf("  Endpoint: %s\n", p.Endpoint)
	fmt.Printf("  Model pool: %d configured\n", len(parseModelList(p.Model)))
	fmt.Printf("  Effort: %s\n", providerEffortSummary(p))
	fmt.Printf("  Context/Compact: %s\n", providerOneMSummary(p))
	printProviderModelMappings(p)
	printProviderExperienceWarnings(p)

	configuredProvider := p
	p, cleanup, err := prepareProviderRuntime(p)
	if err != nil {
		fmt.Printf("\nConnectivity\n  ✗ %v\n", err)
		return nil
	}
	defer cleanup()

	// 5. Test Endpoint reachability and API Authentication key
	if p.Endpoint != "" {
		endpointReachable := false
		fmt.Printf("\nConnectivity\n")
		fmt.Printf("  Checking endpoint and credentials...\n")
		client := http.Client{
			Timeout: 5 * time.Second,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		modelsURL := protocol.NormalizeOpenAIModelsURL(p.Endpoint)
		if provider.IsAnthropicType(p.Type) {
			modelsURL = protocol.NormalizeAnthropicModelsURL(p.Endpoint)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
		if err != nil {
			fmt.Printf("  ✗ Failed to create validation request: %v\n", err)
		} else {
			setProviderAuthHeaders(req, p)

			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("  ✗ Endpoint is unreachable: %v\n", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					fmt.Printf("  ✓ Connected and verified (HTTP %d)\n", resp.StatusCode)
					endpointReachable = true
				} else if resp.StatusCode == http.StatusUnauthorized {
					fmt.Printf("  ✗ Authentication failed (HTTP %d). Verify the API key.\n", resp.StatusCode)
				} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
					// Fallback strategy if GET models returns 404 or 403 on third-party proxies
					fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer fallbackCancel()

					fallbackReq, fallbackErr := http.NewRequestWithContext(fallbackCtx, "GET", p.Endpoint, nil)
					if fallbackErr != nil {
						fmt.Printf("  ✗ Models discovery returned HTTP %d. Failed to create fallback request: %v\n", resp.StatusCode, fallbackErr)
					} else {
						setProviderAuthHeaders(fallbackReq, p)

						fallbackResp, fallbackErr := client.Do(fallbackReq)
						if fallbackErr != nil {
							fmt.Printf("  ✗ Models discovery returned HTTP %d; base endpoint fallback is unreachable: %v\n", resp.StatusCode, fallbackErr)
						} else {
							defer fallbackResp.Body.Close()
							if fallbackResp.StatusCode == http.StatusUnauthorized || fallbackResp.StatusCode == http.StatusForbidden {
								fmt.Printf("  ✗ Authentication failed. Base endpoint returned HTTP %d. Verify the API key.\n", fallbackResp.StatusCode)
							} else {
								fmt.Printf("  ✓ Connected and verified (HTTP %d, models discovery bypassed)\n", resp.StatusCode)
								endpointReachable = true
							}
						}
					}
				} else {
					fmt.Printf("  ! Connected, but returned unexpected status (HTTP %d)\n", resp.StatusCode)
				}
			}
		}

		// 6. Validate configured models with concurrent API calls and reorder (available first)
		if endpointReachable && p.Model != "" {
			configuredModels := parseModelList(p.Model)
			if len(configuredModels) > 0 {
				fmt.Printf("\nModel verification\n")
				availableSet := testModelsConcurrently(configuredModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
				available, unavailable := classifyModels(configuredModels, availableSet)
				fmt.Printf("  %s\n", modelVerificationSummary(available, unavailable))
				if len(unavailable) > 0 {
					fmt.Println("  Run `ccl models` to inspect individual model results.")
				}

				// Reorder and save: available first, unavailable last
				reordered := append(available, unavailable...)
				newModel := strings.Join(reordered, ",")
				if newModel != p.Model {
					configuredProvider.Model = newModel
					cfg.Providers[cfg.ActiveProvider] = configuredProvider
					if err := config.Save(cfg); err != nil {
						fmt.Printf("  ✗ Failed to save reordered models: %v\n", err)
					} else {
						fmt.Printf("  ✓ Config updated: available models prioritized.\n")
					}
				}
			}
		}
	}

	return nil
}

// testModelsConcurrently tests multiple models in batches of 50 concurrent workers.
// Each worker sends a lightweight provider-specific POST to verify the model works.
// Returns a set of model IDs that passed the test.
func testModelsConcurrently(models []string, endpoint, apiKey, providerType, anthropicAuth string) map[string]bool {
	const batchSize = 50
	const requestTimeout = 10 * time.Second

	available := make(map[string]bool)
	var mu sync.Mutex
	var completed, okCount, failCount int64
	total := int64(len(models))

	fmt.Printf("  Checking %d model(s)...\n", total)
	defer func() {
		c := atomic.LoadInt64(&completed)
		o := atomic.LoadInt64(&okCount)
		f := atomic.LoadInt64(&failCount)
		fmt.Printf("  Done: %d/%d checked, %d available, %d unavailable\n", c, total, o, f)
	}()

	for start := 0; start < len(models); start += batchSize {
		end := start + batchSize
		if end > len(models) {
			end = len(models)
		}
		batch := models[start:end]

		var wg sync.WaitGroup
		for _, model := range batch {
			wg.Add(1)
			go func(m string) {
				defer wg.Done()
				ok := testSingleModel(m, endpoint, apiKey, providerType, anthropicAuth, requestTimeout)
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

func testSingleModel(model, endpoint, apiKey, providerType, anthropicAuth string, timeout time.Duration) bool {
	return testSingleModelContext(context.Background(), model, endpoint, apiKey, providerType, anthropicAuth, timeout)
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

func testSingleOpenAIModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	return testSingleOpenAIModelContext(context.Background(), model, endpoint, apiKey, timeout)
}

func testSingleOpenAIModelContext(parent context.Context, model, endpoint, apiKey string, timeout time.Duration) bool {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", buildChatURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func testSingleAnthropicModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	return testSingleAnthropicModelWithAuth(model, endpoint, apiKey, "x-api-key", timeout)
}

func testSingleAnthropicModelWithAuth(model, endpoint, apiKey, authStyle string, timeout time.Duration) bool {
	return testSingleAnthropicModelWithAuthContext(context.Background(), model, endpoint, apiKey, authStyle, timeout)
}

func testSingleAnthropicModelWithAuthContext(parent context.Context, model, endpoint, apiKey, authStyle string, timeout time.Duration) bool {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", buildAnthropicMessagesURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.EqualFold(authStyle, "bearer") {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func testSingleOpenAIResponsesModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	return testSingleOpenAIResponsesModelContext(context.Background(), model, endpoint, apiKey, timeout)
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

	fmt.Println("  Slot mappings:")
	for _, mapping := range mappings {
		model := mapping.model
		if model == "" {
			model = "(unset)"
		}
		fmt.Printf("    %-10s %s\n", mapping.label+":", model)
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
