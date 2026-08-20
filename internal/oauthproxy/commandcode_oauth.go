package oauthproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Command Code browser OAuth constants mirror the official CLI defaults:
// startPort 5959, 10 port attempts, 10KB callback body limit, a 120s browser
// window, and a 2s pause before re-prompting after an invalid key.
const (
	commandcodeCallbackStartPort    = 5959
	commandcodeCallbackPortAttempts = 10
	commandcodeCallbackBodyLimit    = 10 * 1024
	commandcodeInvalidKeyDelay      = 2 * time.Second
)

var (
	// commandcodeStudioBase is the browser authorization origin. It is a var so
	// tests can point the flow away from the real studio while keeping the
	// production default frozen.
	commandcodeStudioBase = "https://commandcode.ai"
	// commandcodeBrowserTimeout bounds the browser callback wait before the
	// flow degrades to manual entry, matching the official 120s race timeout.
	commandcodeBrowserTimeout = 2 * time.Minute
)

// errCommandCodeKeyRejected marks an upstream key rejection so the interactive
// loop can tell an invalid key (re-prompt) apart from a fatal validation
// failure.
var errCommandCodeKeyRejected = errors.New("Command Code key was rejected")

// commandcodeAuthState generates the CSRF state exactly like the official CLI:
// 32 random bytes base64url-encoded (43 characters).
func commandcodeAuthState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Command Code auth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// commandcodeCallbackListener binds the loopback callback server. With an
// explicit port hint (--callback-port) only that port is tried; otherwise the
// official 5959..5968 window is scanned.
func commandcodeCallbackListener(portHint int) (net.Listener, int, error) {
	if portHint > 0 {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portHint))
		if err != nil {
			return nil, 0, fmt.Errorf("bind callback port %d: %w", portHint, err)
		}
		return listener, portHint, nil
	}
	var lastErr error
	for attempt := 0; attempt < commandcodeCallbackPortAttempts; attempt++ {
		port := commandcodeCallbackStartPort + attempt
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return listener, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no available port found after %d attempts starting from port %d: %w",
		commandcodeCallbackPortAttempts, commandcodeCallbackStartPort, lastErr)
}

// commandcodeCallback carries the four identity fields the studio page posts
// back through /callback. The browser path stores them verbatim, exactly like
// the official buildBrowserCommandAuthConfig.
type commandcodeCallback struct {
	apiKey   string
	userID   string
	userName string
	keyName  string
}

// commandcodeCallbackOriginAllowed mirrors the official CORS allow-list.
func commandcodeCallbackOriginAllowed(origin string) bool {
	switch origin {
	case "http://localhost:3000", "https://staging.commandcode.ai", "https://commandcode.ai":
		return true
	default:
		return false
	}
}

// commandcodeCallbackHandler serves POST /callback with the exact contract of
// the official createAuthServer: CORS echo for whitelisted origins, OPTIONS
// preflight, a 10KB body limit, access_denied rejection, strict field checks,
// state comparison, and a final {success:true} before the caller resumes.
func commandcodeCallbackHandler(state string, results chan<- commandcodeCallback, serveErrors chan<- error) http.Handler {
	writeJSON := func(writer http.ResponseWriter, status int, body string) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		if !commandcodeCallbackOriginAllowed(origin) {
			origin = "http://localhost:3000"
		}
		writer.Header().Set("Access-Control-Allow-Origin", origin)
		writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodOptions:
			writer.WriteHeader(http.StatusNoContent)
			return
		case request.URL.Path != "/callback":
			writeJSON(writer, http.StatusNotFound, `{"success":false,"error":"Not found"}`)
			return
		case request.Method != http.MethodPost:
			writeJSON(writer, http.StatusMethodNotAllowed, `{"success":false,"error":"Method not allowed. Use POST."}`)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, commandcodeCallbackBodyLimit)
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, `{"success":false,"error":"Invalid JSON"}`)
			return
		}
		// Error is a pointer: the official CLI checks for key presence, so an
		// explicit empty error also takes the rejection branch.
		var body struct {
			APIKey           string  `json:"apiKey"`
			State            string  `json:"state"`
			UserID           string  `json:"userId"`
			UserName         string  `json:"userName"`
			KeyName          string  `json:"keyName"`
			Error            *string `json:"error"`
			ErrorDescription string  `json:"error_description"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(writer, http.StatusBadRequest, `{"success":false,"error":"Invalid JSON"}`)
			return
		}
		if body.Error != nil {
			writeJSON(writer, http.StatusOK, `{"success":true}`)
			description := body.ErrorDescription
			if *body.Error == "access_denied" {
				if description == "" {
					description = "Authorization was denied by the user"
				}
				serveErrors <- fmt.Errorf("Command Code authorization was denied: %s", description)
			} else {
				if description == "" {
					description = *body.Error
				}
				serveErrors <- fmt.Errorf("Command Code authorization failed: %s", description)
			}
			return
		}
		if body.APIKey == "" || body.State == "" || body.UserID == "" || body.UserName == "" || body.KeyName == "" {
			writeJSON(writer, http.StatusBadRequest, `{"success":false,"error":"Missing required fields"}`)
			return
		}
		if body.State != state {
			writeJSON(writer, http.StatusForbidden, `{"success":false,"error":"Invalid state token"}`)
			return
		}
		writeJSON(writer, http.StatusOK, `{"success":true}`)
		results <- commandcodeCallback{
			apiKey:   body.APIKey,
			userID:   body.UserID,
			userName: body.UserName,
			keyName:  body.KeyName,
		}
	})
}

// commandcodeAuthURL builds the studio authorization URL exactly like the
// official buildCommandAuthUrl, including the URL-encoded loopback callback.
func commandcodeAuthURL(port int, state string) string {
	callback := fmt.Sprintf("http://localhost:%d/callback", port)
	return fmt.Sprintf("%s/studio/auth/cli?callback=%s&state=%s",
		commandcodeStudioBase, url.QueryEscape(callback), url.QueryEscape(state))
}

// commandcodeSanitizePastedKey strips whitespace and the terminal bracketed-
// paste guards ([200~/[201~) around a pasted key.
func commandcodeSanitizePastedKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimSuffix(key, "\x1b[201~")
	key = strings.TrimPrefix(key, "\x1b[200~")
	key = strings.TrimSuffix(key, "[201~")
	key = strings.TrimPrefix(key, "[200~")
	return strings.TrimSpace(key)
}

// commandcodeSubmitKey mirrors the official CLI's submitApiKey: sanitize the
// pasted text, validate through /alpha/whoami, and persist with the
// manual-entry metadata shape. retry is true when the key was invalid and the
// interactive loop should re-prompt; fatal failures return retry=false.
func commandcodeSubmitKey(ctx context.Context, base, authDir, raw string) (LoginResult, bool, error) {
	apiKey := commandcodeSanitizePastedKey(raw)
	if apiKey == "" {
		fmt.Println("That key was invalid. Try again.")
		return LoginResult{}, true, nil
	}
	fmt.Println("Validating API key...")
	user, err := commandcodeWhoami(ctx, base, apiKey)
	if err != nil {
		if errors.Is(err, errCommandCodeKeyRejected) {
			fmt.Println("That key was invalid. Try again.")
			return LoginResult{}, true, nil
		}
		return LoginResult{}, false, err
	}
	userID, userName := "manual-entry", "API Key"
	if user != nil {
		if strings.TrimSpace(user.ID) != "" {
			userID = strings.TrimSpace(user.ID)
		}
		switch {
		case strings.TrimSpace(user.UserName) != "":
			userName = strings.TrimSpace(user.UserName)
		case strings.TrimSpace(user.Name) != "":
			userName = strings.TrimSpace(user.Name)
		}
	}
	metadata := map[string]any{
		"type":             ProviderCommandCode,
		"api_key":          apiKey,
		"user_id":          userID,
		"user_name":        userName,
		"key_name":         "cli-manual-entry",
		"authenticated_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"source":           "manual_paste",
	}
	result, saveErr := saveCommandCodeCredential(authDir, metadata)
	if saveErr != nil {
		return LoginResult{}, false, saveErr
	}
	return result, false, nil
}

// loginCommandCodeOAuth runs the browser OAuth flow that mirrors the official
// CLI login: it opens the studio authorization page, accepts the copied API
// key back either through the loopback callback or as a manual paste, and
// races both against a 2-minute browser window. A rejected key re-prompts
// after a short pause instead of aborting.
func loginCommandCodeOAuth(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state, err := commandcodeAuthState()
	if err != nil {
		return LoginResult{}, err
	}
	listener, port, err := commandcodeCallbackListener(opts.CallbackPort)
	if err != nil {
		return LoginResult{}, err
	}
	results := make(chan commandcodeCallback, 1)
	serveErrors := make(chan error, 2)
	server := &http.Server{
		Handler:           commandcodeCallbackHandler(state, results, serveErrors),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serveErrors <- serveErr
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// Feed pasted lines from stdin (when there is a terminal) concurrently
	// with the browser callback, so both entry modes stay live at once.
	stdinLines := make(chan string, 8)
	if opts.Stdin != nil {
		go func() {
			defer close(stdinLines)
			scanner := bufio.NewScanner(opts.Stdin)
			scanner.Buffer(make([]byte, 64*1024), 1<<20)
			for scanner.Scan() {
				stdinLines <- scanner.Text()
			}
		}()
	}

	authURL := commandcodeAuthURL(port, state)
	fmt.Println("Authorize in browser, or paste API key here")
	fmt.Printf("Get API key: %s\n", authURL)
	if !opts.NoBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Printf("Could not open browser. Paste your API key below: %v\n", err)
		}
		fmt.Println("If your browser doesn't open, go to this link.")
	}
	fmt.Println("Waiting for browser authorization, or paste your Command Code API key and press Enter:")

	base := commandcodeAPIBase("")
	timeout := time.NewTimer(commandcodeBrowserTimeout)
	defer timeout.Stop()
	for {
		select {
		case callback := <-results:
			metadata := map[string]any{
				"type":             ProviderCommandCode,
				"api_key":          callback.apiKey,
				"user_id":          callback.userID,
				"user_name":        callback.userName,
				"key_name":         callback.keyName,
				"authenticated_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				"source":           "browser_callback",
			}
			return saveCommandCodeCredential(authDir, metadata)
		case serveErr := <-serveErrors:
			return LoginResult{}, fmt.Errorf("serve Command Code callback: %w", serveErr)
		case keyText, ok := <-stdinLines:
			if !ok {
				stdinLines = nil // stdin exhausted: only the browser can finish
				continue
			}
			result, retry, submitErr := commandcodeSubmitKey(ctx, base, authDir, keyText)
			if submitErr != nil {
				return LoginResult{}, submitErr
			}
			if !retry {
				return result, nil
			}
			// Official CLI pauses 2s after a rejected key before re-prompting.
			select {
			case <-time.After(commandcodeInvalidKeyDelay):
				fmt.Println("Paste a valid API key below.")
			case <-ctx.Done():
				return LoginResult{}, fmt.Errorf("Command Code login: %w", ctx.Err())
			}
		case <-timeout.C:
			// Browser window expired: stop the callback server and degrade to
			// manual entry, exactly like the official race timeout.
			_ = server.Close()
			fmt.Println("Browser auth timed out. Paste your API key below.")
		case <-ctx.Done():
			return LoginResult{}, fmt.Errorf("Command Code login: %w", ctx.Err())
		}
	}
}
