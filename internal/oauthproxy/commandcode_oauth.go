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
	"sync"
	"sync/atomic"
	"syscall"
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
// official 5959..5968 window is scanned. Because the callback URL uses the
// name localhost, a usable port must be owned on both IPv4 and IPv6 when IPv6
// loopback is available.
func commandcodeCallbackListener(portHint int) ([]net.Listener, int, error) {
	bindPort := func(port int) ([]net.Listener, error) {
		v4, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, err
		}
		v6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port))
		if err == nil {
			return []net.Listener{v4, v6}, nil
		}
		if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			return []net.Listener{v4}, nil
		}
		_ = v4.Close()
		return nil, err
	}
	if portHint > 0 {
		listeners, err := bindPort(portHint)
		if err != nil {
			return nil, 0, fmt.Errorf("bind callback port %d: %w", portHint, err)
		}
		return listeners, portHint, nil
	}
	var lastErr error
	for attempt := range commandcodeCallbackPortAttempts {
		port := commandcodeCallbackStartPort + attempt
		listeners, err := bindPort(port)
		if err == nil {
			return listeners, port, nil
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

// commandcodeTrySendCallback delivers a callback without allowing an HTTP
// handler to wait for the login loop. The parent owns channel lifetime; a
// closed done channel and a full buffer both mean the callback is stale.
func commandcodeTrySendCallback(done <-chan struct{}, results chan<- commandcodeCallback, callback commandcodeCallback) bool {
	select {
	case <-done:
		return false
	default:
	}
	select {
	case results <- callback:
		return true
	case <-done:
		return false
	default:
		return false
	}
}

func commandcodeTrySendError(done <-chan struct{}, errors chan<- error, err error) bool {
	select {
	case <-done:
		return false
	default:
	}
	select {
	case errors <- err:
		return true
	case <-done:
		return false
	default:
		return false
	}
}

// commandcodeCallbackHandler serves POST /callback with the exact contract of
// the official createAuthServer: CORS echo for whitelisted origins, OPTIONS
// preflight, a 10KB body limit, access_denied rejection, strict field checks,
// state comparison, and a final {success:true} before the caller resumes.
//
// The public wrapper is retained for focused handler tests. Login uses the
// done-aware variant so callbacks racing with timeout/shutdown get a bounded
// response instead of blocking on a result channel.
func commandcodeCallbackHandler(state string, results chan<- commandcodeCallback, serveErrors chan<- error) http.Handler {
	return commandcodeCallbackHandlerWithDone(state, results, serveErrors, nil)
}

func commandcodeCallbackHandlerWithDone(state string, results chan<- commandcodeCallback, serveErrors chan<- error, done <-chan struct{}) http.Handler {
	return commandcodeCallbackHandlerWithDoneAndMutex(state, results, serveErrors, done, nil)
}

// commandcodeCallbackHandlerWithDoneAndMutex is the login-loop variant of the
// callback handler. deliveryMu is shared with the owner that closes done, so a
// callback cannot enqueue work after shutdown has begun. The mutex also keeps
// the success response ahead of the owner's server close when a callback wins.
func commandcodeCallbackHandlerWithDoneAndMutex(state string, results chan<- commandcodeCallback, serveErrors chan<- error, done <-chan struct{}, deliveryMu *sync.Mutex) http.Handler {
	var accepted atomic.Bool
	writeJSON := func(writer http.ResponseWriter, status int, body string) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}
	withDelivery := func(fn func() bool) bool {
		if deliveryMu != nil {
			deliveryMu.Lock()
			defer deliveryMu.Unlock()
		}
		return fn()
	}
	sendErrorResponse := func(writer http.ResponseWriter, err error) bool {
		return withDelivery(func() bool {
			if !commandcodeTrySendError(done, serveErrors, err) {
				writeJSON(writer, http.StatusGone, `{"success":false,"error":"Authorization session closed"}`)
				return false
			}
			writeJSON(writer, http.StatusOK, `{"success":true}`)
			return true
		})
	}
	sendCallbackResponse := func(writer http.ResponseWriter, callback commandcodeCallback) bool {
		return withDelivery(func() bool {
			if !commandcodeTrySendCallback(done, results, callback) {
				writeJSON(writer, http.StatusGone, `{"success":false,"error":"Authorization session closed"}`)
				return false
			}
			writeJSON(writer, http.StatusOK, `{"success":true}`)
			return true
		})
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
		select {
		case <-done:
			writeJSON(writer, http.StatusGone, `{"success":false,"error":"Authorization session closed"}`)
			return
		default:
		}
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
		// Error callbacks are authenticated by the same state as successful
		// callbacks. Checking it first prevents unauthenticated requests from
		// consuming the one-shot login or injecting text into the terminal.
		if body.State != state {
			writeJSON(writer, http.StatusForbidden, `{"success":false,"error":"Invalid state token"}`)
			return
		}
		if body.Error != nil {
			if !accepted.CompareAndSwap(false, true) {
				writeJSON(writer, http.StatusConflict, `{"success":false,"error":"Authorization already completed"}`)
				return
			}
			description := commandcodeSanitizeErrorText(body.ErrorDescription)
			if *body.Error == "access_denied" {
				if description == "" {
					description = "Authorization was denied by the user"
				}
			} else if description == "" {
				description = commandcodeSanitizeErrorText(*body.Error)
			}
			if *body.Error == "access_denied" {
				sendErrorResponse(writer, fmt.Errorf("Command Code authorization was denied: %s", description))
			} else {
				sendErrorResponse(writer, fmt.Errorf("Command Code authorization failed: %s", description))
			}
			return
		}
		if body.APIKey == "" || body.State == "" || body.UserID == "" || body.UserName == "" || body.KeyName == "" {
			writeJSON(writer, http.StatusBadRequest, `{"success":false,"error":"Missing required fields"}`)
			return
		}
		if !accepted.CompareAndSwap(false, true) {
			writeJSON(writer, http.StatusConflict, `{"success":false,"error":"Authorization already completed"}`)
			return
		}
		if !sendCallbackResponse(writer, commandcodeCallback{
			apiKey:   body.APIKey,
			userID:   body.UserID,
			userName: body.UserName,
			keyName:  body.KeyName,
		}) {
			return
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

// commandcodeSanitizeErrorText removes terminal control characters before a
// callback error is returned to the interactive CLI. Newlines are flattened
// so an error cannot forge additional terminal output lines.
func commandcodeSanitizeErrorText(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		default:
			return r
		}
	}, value)
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
	metadata := commandcodeCredentialMetadata(apiKey, user, map[string]any{
		"key_name":         "cli-manual-entry",
		"authenticated_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"source":           "manual_paste",
	})
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
func loginCommandCodeOAuth(ctx context.Context, authDir string, opts LoginOptions) (result LoginResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	loginCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	state, err := commandcodeAuthState()
	if err != nil {
		return LoginResult{}, err
	}
	listeners, port, err := commandcodeCallbackListener(opts.CallbackPort)
	if err != nil {
		return LoginResult{}, err
	}

	results := make(chan commandcodeCallback, 1)
	serveErrors := make(chan error, 2)
	callbackDone := make(chan struct{})
	var deliveryMu sync.Mutex
	var stopCallbackOnce sync.Once
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	stopCallbackServer := func() {
		stopCallbackOnce.Do(func() {
			// The callback handler holds deliveryMu while enqueueing a result and
			// writing its success response. Closing the session under the same
			// mutex prevents a queued callback from racing with shutdown and keeps
			// the browser response ahead of server.Close.
			deliveryMu.Lock()
			close(callbackDone)
			_ = server.Close()
			deliveryMu.Unlock()
		})
	}
	server.Handler = commandcodeCallbackHandlerWithDoneAndMutex(state, results, serveErrors, callbackDone, &deliveryMu)
	serveDone := make(chan struct{})
	var serveWG sync.WaitGroup
	serveWG.Add(len(listeners))
	for _, listener := range listeners {
		go func(listener net.Listener) {
			defer serveWG.Done()
			if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				deliveryMu.Lock()
				commandcodeTrySendError(callbackDone, serveErrors, serveErr)
				deliveryMu.Unlock()
			}
		}(listener)
	}
	go func() {
		serveWG.Wait()
		close(serveDone)
	}()

	stdinLines := make(chan string, 8)
	stdinDone := make(chan struct{})
	if opts.Stdin != nil {
		go func() {
			defer close(stdinDone)
			scanner := bufio.NewScanner(opts.Stdin)
			scanner.Buffer(make([]byte, 64*1024), 1<<20)
			for scanner.Scan() {
				select {
				case stdinLines <- scanner.Text():
				case <-loginCtx.Done():
					return
				}
			}
			if scanErr := scanner.Err(); scanErr != nil {
				deliveryMu.Lock()
				commandcodeTrySendError(callbackDone, serveErrors, fmt.Errorf("read Command Code login input: %w", scanErr))
				deliveryMu.Unlock()
			}
			close(stdinLines)
		}()
	} else {
		close(stdinDone)
		close(stdinLines)
	}

	// All exit paths use the same cleanup. The optional cancellation hook can
	// interrupt a reader owned by the caller; Login deliberately does not close
	// an io.Closer implicitly because cmd.InOrStdin() commonly returns os.Stdin.
	defer func() {
		cancel()
		stopCallbackServer()
		if opts.StdinCancel != nil {
			opts.StdinCancel()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Shutdown(shutdownCtx)
		shutdownCancel()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
		}
		select {
		case <-stdinDone:
		case <-time.After(time.Second):
		}
	}()

	authURL := commandcodeAuthURL(port, state)
	fmt.Println("Authorize in browser, or paste API key here")
	fmt.Printf("Get API key: %s\n", authURL)
	if !opts.NoBrowser {
		if browserErr := openBrowser(authURL); browserErr != nil {
			fmt.Printf("Could not open browser. Paste your API key below: %v\n", browserErr)
		}
		fmt.Println("If your browser doesn't open, go to this link.")
	}
	fmt.Println("Waiting for browser authorization, or paste your Command Code API key and press Enter:")

	base := commandcodeAPIBase("")
	timeout := time.NewTimer(commandcodeBrowserTimeout)
	defer timeout.Stop()
	var stdinChannel <-chan string
	if opts.Stdin != nil {
		stdinChannel = stdinLines
	}
	browserClosed := false
	for {
		select {
		case callback := <-results:
			stopCallbackServer()
			apiKey := commandcodeSanitizePastedKey(callback.apiKey)
			if apiKey == "" {
				if stdinChannel == nil {
					return LoginResult{}, errors.New("Command Code callback returned an empty API key")
				}
				fmt.Println("The browser-provided key was empty. Paste a valid API key below.")
				continue
			}
			user, validateErr := commandcodeValidateCredential(loginCtx, base, apiKey)
			if validateErr != nil {
				if errors.Is(validateErr, errCommandCodeKeyRejected) {
					if stdinChannel == nil {
						return LoginResult{}, validateErr
					}
					fmt.Println("The browser-provided key was invalid. Paste a valid API key below.")
					continue
				}
				return LoginResult{}, validateErr
			}
			metadata := commandcodeCredentialMetadata(apiKey, user, map[string]any{
				"key_name":         callback.keyName,
				"authenticated_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				"source":           "browser_callback",
			})
			return saveCommandCodeCredential(authDir, metadata)
		case serveErr := <-serveErrors:
			return LoginResult{}, fmt.Errorf("serve Command Code callback: %w", serveErr)
		case keyText, ok := <-stdinChannel:
			if !ok {
				return LoginResult{}, errors.New("Command Code login ended: stdin reached EOF before authorization completed")
			}
			result, retry, submitErr := commandcodeSubmitKey(loginCtx, base, authDir, keyText)
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
			case <-loginCtx.Done():
				return LoginResult{}, fmt.Errorf("Command Code login: %w", loginCtx.Err())
			}
		case <-timeout.C:
			if !browserClosed {
				browserClosed = true
				stopCallbackServer()
				fmt.Println("Browser auth timed out. Paste your API key below.")
				if stdinChannel == nil {
					return LoginResult{}, errors.New("Command Code browser authorization timed out and no manual input is available")
				}
			}
		case <-loginCtx.Done():
			return LoginResult{}, fmt.Errorf("Command Code login: %w", loginCtx.Err())
		}
	}
}
