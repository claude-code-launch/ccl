package claude

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/haiboyuwen/claude-code-launch/internal/provider"
	"github.com/haiboyuwen/claude-code-launch/internal/proxy"
)

func Run(p provider.Provider, args []string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI is not installed or not in PATH. Install with: npm install -g @anthropic-ai/claude-code. Error: %w", err)
	}

	var srv *proxy.Server
	var baseURL string

	// If the provider type is "openai", start the local translation proxy
	needsProxy := p.Type == "openai" || (p.Type == "anthropic" && p.Endpoint != "" && p.Endpoint != "https://api.anthropic.com")
	if needsProxy {
		// Initialize completely silent logger for the background proxy to prevent polluting terminal UI
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		proxyAddr := "127.0.0.1:0"
		srv = proxy.NewServer(proxyAddr, p, logger)

		err := srv.Start()
		if err != nil {
			return fmt.Errorf("failed to start local proxy: %w", err)
		}
		defer srv.Stop()

		// Retrieve the dynamically allocated address (e.g. 127.0.0.1:52184)
		allocatedAddr := srv.Addr()

		// Set the base URL to our local proxy instead of the real endpoint
		baseURL = "http://" + allocatedAddr

		// Give the proxy server a brief moment to bind and start listening
		time.Sleep(200 * time.Millisecond)
	} else {
		baseURL = p.Endpoint
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// On Windows, the globally installed npm binary "claude" is a batch file or ps1 script (claude.cmd / claude.ps1).
		// We must invoke it through cmd.exe to ensure proper command parsing and shell execution.
		winArgs := append([]string{"/c", claudePath}, args...)
		cmd = exec.Command("cmd", winArgs...)
	} else {
		cmd = exec.Command(claudePath, args...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Env = os.Environ()

	// Inject base URL (either the local proxy or direct anthropic endpoint)
	if baseURL != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+baseURL)
	}

	// API key can be set directly.
	// NOTE: For OpenAI providers, Claude Code doesn't need to know the real API key,
	// because the local proxy intercepts and adds the real Bearer token.
	// However, we inject a dummy key so Claude Code doesn't complain about missing keys.
	if needsProxy {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=local-proxy-dummy-key")
	} else if p.APIKey != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY="+p.APIKey)
	}

	if p.Model != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_MODEL="+p.Model)
	}

	return cmd.Run()
}
