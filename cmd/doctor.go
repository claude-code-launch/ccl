package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites and provider connectivity",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Diagnosing ccl environment:")

		// 1. Check Node.js
		nodePath, err := exec.LookPath("node")
		if err != nil {
			fmt.Println("- Info: Node.js is not installed (no longer required by newer native Claude Code binaries)")
		} else {
			fmt.Printf("✓ Node.js found at: %s\n", nodePath)
		}

		// 2. Check Claude CLI
		claudeInstalled := IsInstalled()
		if !claudeInstalled {
			fmt.Println("✗ Claude Code CLI is not installed or not in PATH.")
			// Prompt to install automatically
			err := AutoInstall()
			if err != nil {
				fmt.Printf("✗ Auto-installation failed: %v. Please install manually by visiting: https://code.claude.com/\n", err)
			}
		} else {
			claudePath, _ := exec.LookPath("claude")
			fmt.Printf("✓ Claude Code CLI installed at: %s\n", claudePath)
		}

		// 3. Check Configuration File
		cfg, err := config.Load()
		if err != nil {
			fmt.Printf("✗ Failed to load config: %v\n", err)
			return nil
		}
		fmt.Printf("✓ Config file found at: %s\n", config.ConfigPath())

		// 4. Check Active Provider
		if cfg.ActiveProvider == "" {
			fmt.Println("✗ Active provider is not selected. Use 'ccl set' or 'ccl use'")
			return nil
		}
		fmt.Printf("✓ Active provider: %s\n", cfg.ActiveProvider)

		p, ok := cfg.Providers[cfg.ActiveProvider]
		if !ok {
			fmt.Printf("✗ Selected provider %q does not exist in config\n", cfg.ActiveProvider)
			return nil
		}

		fmt.Printf("  - Type: %s\n", provider.ProtocolLabel(p.Type))
		fmt.Printf("  - Auth: %s\n", providerAuthLabel(p))
		fmt.Printf("  - Endpoint: %s\n", p.Endpoint)
		fmt.Printf("  - Model: %s\n", p.Model)
		fmt.Printf("  - Effort: %s\n", providerEffortSummary(p))
		fmt.Printf("  - 1M Context: %s\n", providerOneMSummary(p))
		printProviderExperienceWarnings(p)

		// 5. Test Endpoint reachability and API Authentication key
		if p.Endpoint != "" {
			endpointReachable := false
			fmt.Printf("  - Testing reachability and auth key validation...\n")
			client := http.Client{
				Timeout: 5 * time.Second,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			modelsURL := protocol.NormalizeOpenAIModelsURL(p.Endpoint)
			if p.Type == "anthropic" {
				modelsURL = protocol.NormalizeAnthropicModelsURL(p.Endpoint)
			}

			req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
			if err != nil {
				fmt.Printf("\n✗ Failed to create validation request: %v\n", err)
			} else {
				setProviderAuthHeaders(req, p)

				resp, err := client.Do(req)
				if err != nil {
					fmt.Printf("\n✗ Endpoint is unreachable: %v\n", err)
				} else {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						fmt.Printf(" Success! Connected and verified. (HTTP %d)\n", resp.StatusCode)
						endpointReachable = true
					} else if resp.StatusCode == http.StatusUnauthorized {
						fmt.Printf("\n✗ Authentication failed! (HTTP %d). Please verify your API Key.\n", resp.StatusCode)
					} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
						// Fallback strategy if GET models returns 404 or 403 on third-party proxies
						fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer fallbackCancel()

						fallbackReq, fallbackErr := http.NewRequestWithContext(fallbackCtx, "GET", p.Endpoint, nil)
						if fallbackErr != nil {
							fmt.Printf("\n✗ Models discovery returned HTTP %d. Failed to create fallback request: %v\n", resp.StatusCode, fallbackErr)
						} else {
							setProviderAuthHeaders(fallbackReq, p)

							fallbackResp, fallbackErr := client.Do(fallbackReq)
							if fallbackErr != nil {
								fmt.Printf("\n✗ Models discovery returned HTTP %d, and base endpoint fallback is unreachable: %v\n", resp.StatusCode, fallbackErr)
							} else {
								defer fallbackResp.Body.Close()
								if fallbackResp.StatusCode == http.StatusUnauthorized || fallbackResp.StatusCode == http.StatusForbidden {
									fmt.Printf("\n✗ Authentication failed! Base endpoint returned HTTP %d. Please verify your API Key.\n", fallbackResp.StatusCode)
								} else {
									fmt.Printf(" Success! Connected and verified. (HTTP %d, models discovery bypassed)\n", resp.StatusCode)
									endpointReachable = true
								}
							}
						}
					} else {
						fmt.Printf(" Connected, but returned unexpected status (HTTP %d)\n", resp.StatusCode)
					}
				}
			}

			// 6. Validate configured models with concurrent API calls and reorder (available first)
			if endpointReachable && p.Model != "" {
				configuredModels := parseModelList(p.Model)
				if len(configuredModels) > 0 {
					fmt.Printf("\n  - Validating %d configured model(s) with concurrent tests...\n", len(configuredModels))
					availableSet := testModelsConcurrently(configuredModels, p.Endpoint, p.APIKey, p.Type, p.AnthropicAuth)
					available, unavailable := classifyModels(configuredModels, availableSet)
					printModelReport(available, unavailable)

					// Reorder and save: available first, unavailable last
					reordered := append(available, unavailable...)
					newModel := strings.Join(reordered, ",")
					if newModel != p.Model {
						p.Model = newModel
						cfg.Providers[cfg.ActiveProvider] = p
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
	},
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

	// Progress bar ticker
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c := atomic.LoadInt64(&completed)
				o := atomic.LoadInt64(&okCount)
				f := atomic.LoadInt64(&failCount)
				pct := int(float64(c) / float64(total) * 100)
				bar := buildProgressBar(30, pct)
				fmt.Printf("\r  %s %d/%d ✓%d ✗%d", bar, c, total, o, f)
			}
		}
	}()
	defer func() {
		close(done)
		// Print final 100%% bar
		c := atomic.LoadInt64(&completed)
		o := atomic.LoadInt64(&okCount)
		f := atomic.LoadInt64(&failCount)
		bar := buildProgressBar(30, 100)
		fmt.Printf("\r  %s %d/%d ✓%d ✗%d\n", bar, c, total, o, f)
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

// buildProgressBar returns a visual progress bar string.
func buildProgressBar(width int, pct int) string {
	filled := pct * width / 100
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < width; i++ {
		if i < filled {
			sb.WriteString("█")
		} else {
			sb.WriteString("░")
		}
	}
	sb.WriteByte(']')
	return sb.String()
}
func testSingleModel(model, endpoint, apiKey, providerType, anthropicAuth string, timeout time.Duration) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	if providerType == "anthropic" {
		if strings.TrimSpace(anthropicAuth) == "" {
			anthropicAuth = "x-api-key"
		}
		return testSingleAnthropicModelWithAuth(model, endpoint, apiKey, anthropicAuth, timeout)
	}
	if provider.IsOpenAIResponsesType(providerType) {
		return testSingleOpenAIResponsesModel(model, endpoint, apiKey, timeout)
	}
	return testSingleOpenAIModel(model, endpoint, apiKey, timeout)
}

func testSingleOpenAIModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func testSingleAnthropicModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	return testSingleAnthropicModelWithAuth(model, endpoint, apiKey, "x-api-key", timeout)
}

func testSingleAnthropicModelWithAuth(model, endpoint, apiKey, authStyle string, timeout time.Duration) bool {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func testSingleOpenAIResponsesModel(model, endpoint, apiKey string, timeout time.Duration) bool {
	return protocol.ProbeOpenAIResponsesSupport(endpoint, apiKey, model, timeout)
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

// printModelReport displays which models are available and which are not.
func printModelReport(available, unavailable []string) {
	for _, m := range available {
		fmt.Printf("    ✓ %s\n", m)
	}
	for _, m := range unavailable {
		fmt.Printf("    ✗ %s (unavailable)\n", m)
	}
	if len(available) > 0 && len(unavailable) > 0 {
		fmt.Printf("  %d available, %d unavailable\n", len(available), len(unavailable))
	} else if len(available) > 0 {
		fmt.Printf("  All %d model(s) available.\n", len(available))
	} else if len(unavailable) > 0 {
		fmt.Printf("  All %d model(s) unavailable - check endpoint and API key.\n", len(unavailable))
	}
}

func RootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
