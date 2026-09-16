package oauthproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	autoclawOAuthCaptchaConfigPath = "/userapi/overseasv1/oauth-captcha-config"
	autoclawGoogleOAuthURLPath     = "/userapi/overseasv1/google-oauth-url"
	autoclawGoogleOAuthLoginPath   = "/userapi/overseasv1/google-oauth-login"
	autoclawOAuthCallbackPath      = "/auth/callback-google"
	autoclawOAuthLoginPagePath     = "/autoclaw/oauth"
	autoclawOAuthURLPath           = "/autoclaw/oauth-url"
	autoclawOAuthTimeout           = 5 * time.Minute
	autoclawOAuthRequestTimeout    = 30 * time.Second
	autoclawCaptchaScriptURL       = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
)

var autoclawBrowserOpener = openBrowser

type autoClawCaptchaConfig struct {
	Enabled  bool   `json:"enabled"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix"`
	SceneID  string `json:"scene_id"`
	Supplier string `json:"captcha_supplier"`
}

type autoClawOAuthCallback struct {
	code  string
	state string
	err   string
}

type autoClawOAuthState struct {
	mu       sync.RWMutex
	expected string
}

func (s *autoClawOAuthState) set(value string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.expected = strings.TrimSpace(value)
	s.mu.Unlock()
}

func (s *autoClawOAuthState) matches(value string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	expected := s.expected
	s.mu.RUnlock()
	value = strings.TrimSpace(value)
	return expected != "" && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

type autoClawOAuthURLResponse struct {
	OAuthURL string `json:"oauth_url"`
}

type autoClawOAuthLoginResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	UserID       json.RawMessage `json:"user_id"`
	UserName     string          `json:"user_name"`
	Email        string          `json:"email"`
}

// loginAutoClaw runs AutoClaw's Google OAuth flow without starting the desktop
// application. CCL owns the loopback callback and a small local page that hosts
// the same Aliyun human-verification widget used by AutoClaw. The resulting
// access and refresh tokens are stored under ~/.ccl/auth and refreshed by CCL.
func loginAutoClaw(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deviceID, err := randomAutoClawHex(32)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create AutoClaw device id: %w", err)
	}
	localNonce, err := randomAutoClawHex(24)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create AutoClaw login nonce: %w", err)
	}
	version := autoClawInstalledVersion()
	client := &http.Client{Timeout: autoclawOAuthRequestTimeout}
	captchaConfig, err := fetchAutoClawCaptchaConfig(ctx, client, version)
	if err != nil {
		return LoginResult{}, err
	}
	if captchaConfig.Enabled {
		supplier := strings.ToLower(strings.TrimSpace(captchaConfig.Supplier))
		if supplier == "" {
			supplier = "aliyun"
		}
		if supplier != "aliyun" {
			return LoginResult{}, fmt.Errorf("AutoClaw OAuth requires unsupported %s captcha; use `ccl import autoclaw` after signing in with the desktop app", supplier)
		}
		if strings.TrimSpace(captchaConfig.Region) == "" || strings.TrimSpace(captchaConfig.Prefix) == "" || strings.TrimSpace(captchaConfig.SceneID) == "" {
			return LoginResult{}, errors.New("AutoClaw OAuth returned an incomplete Aliyun captcha configuration")
		}
	}

	listenerAddress := "127.0.0.1:0"
	if opts.CallbackPort > 0 {
		listenerAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.CallbackPort))
	}
	listener, err := net.Listen("tcp", listenerAddress)
	if err != nil {
		return LoginResult{}, fmt.Errorf("listen for AutoClaw OAuth callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	navigateURI := fmt.Sprintf("http://localhost:%d%s", port, autoclawOAuthCallbackPath)
	callbackCh := make(chan autoClawOAuthCallback, 1)
	oauthState := &autoClawOAuthState{}
	serverErrors := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(autoclawOAuthLoginPagePath, autoClawOAuthPageHandler(captchaConfig, localNonce))
	mux.HandleFunc(autoclawOAuthURLPath, autoClawOAuthURLHandler(client, version, deviceID, navigateURI, localNonce, captchaConfig, oauthState))
	mux.HandleFunc(autoclawOAuthCallbackPath, autoClawOAuthCallbackHandler(callbackCh, oauthState))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case serverErrors <- serveErr:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	loginPageURL := fmt.Sprintf("http://127.0.0.1:%d%s?nonce=%s", port, autoclawOAuthLoginPagePath, url.QueryEscape(localNonce))
	fmt.Printf("Open %s to authorize AutoClaw\n", loginPageURL)
	if !opts.NoBrowser {
		if openErr := autoclawBrowserOpener(loginPageURL); openErr != nil {
			fmt.Printf("Could not open a browser automatically; open the URL above manually: %v\n", openErr)
		}
	}
	fmt.Println("Waiting for AutoClaw authentication callback...")

	loginCtx, cancel := context.WithTimeout(ctx, autoclawOAuthTimeout)
	defer cancel()
	var callback autoClawOAuthCallback
	select {
	case callback = <-callbackCh:
	case serveErr := <-serverErrors:
		return LoginResult{}, fmt.Errorf("serve AutoClaw OAuth callback: %w", serveErr)
	case <-loginCtx.Done():
		return LoginResult{}, fmt.Errorf("AutoClaw authentication: %w", loginCtx.Err())
	}
	if callback.err != "" {
		return LoginResult{}, fmt.Errorf("AutoClaw authentication failed: %s", callback.err)
	}
	if callback.code == "" || callback.state == "" {
		return LoginResult{}, errors.New("AutoClaw authentication failed: callback is missing code or state")
	}

	loginData, err := exchangeAutoClawGoogleCode(loginCtx, client, version, deviceID, navigateURI, callback)
	if err != nil {
		return LoginResult{}, err
	}
	metadata := autoClawMetadataFromOAuth(loginData, deviceID, version)
	result, err := saveAutoClawCredential(authDir, metadata)
	if err != nil {
		return LoginResult{}, err
	}
	fmt.Println("AutoClaw authentication successful; CCL will refresh this session independently")
	return result, nil
}

func randomAutoClawHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func fetchAutoClawCaptchaConfig(ctx context.Context, client *http.Client, version string) (autoClawCaptchaConfig, error) {
	result, err := autoClawOAuthRequest(ctx, client, version, autoclawOAuthCaptchaConfigPath, map[string]any{})
	if err != nil {
		return autoClawCaptchaConfig{}, fmt.Errorf("get AutoClaw captcha configuration: %w", err)
	}
	if result.code != 0 {
		return autoClawCaptchaConfig{}, fmt.Errorf("get AutoClaw captcha configuration: code %d: %s", result.code, result.message)
	}
	var config autoClawCaptchaConfig
	if len(bytes.TrimSpace(result.data)) == 0 || string(bytes.TrimSpace(result.data)) == "null" {
		return config, nil
	}
	if err := json.Unmarshal(result.data, &config); err != nil {
		return config, fmt.Errorf("decode AutoClaw captcha configuration: %w", err)
	}
	return config, nil
}

func autoClawOAuthRequest(ctx context.Context, client *http.Client, version, path string, payload any) (autoClawRefreshResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return autoClawRefreshResult{}, err
	}
	headers := autoClawSignedHeaders(time.Now().Unix(), version)
	headers.Set("User-Agent", "node")
	endpoint := strings.TrimRight(autoclawAPIOrigin, "/") + path
	return autoClawRefreshRequest(ctx, client, endpoint, headers, raw)
}

func autoClawOAuthPageHandler(config autoClawCaptchaConfig, expectedNonce string) http.HandlerFunc {
	configJSON, _ := json.Marshal(config)
	encodedConfig := base64.StdEncoding.EncodeToString(configJSON)
	page := autoClawOAuthPageHTML(encodedConfig, expectedNonce)
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || subtle.ConstantTimeCompare([]byte(request.URL.Query().Get("nonce")), []byte(expectedNonce)) != 1 {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("X-Frame-Options", "DENY")
		_, _ = io.WriteString(writer, page)
	}
}

func autoClawOAuthURLHandler(client *http.Client, version, deviceID, navigateURI, expectedNonce string, config autoClawCaptchaConfig, oauthState *autoClawOAuthState) http.HandlerFunc {
	type requestBody struct {
		VerifyParam string `json:"ali_captcha_verify_param"`
	}
	type responseBody struct {
		OK       bool   `json:"ok"`
		OAuthURL string `json:"oauth_url,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	return func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(request.Header.Get("X-CCL-OAuth-Nonce")), []byte(expectedNonce)) != 1 {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: "not found"})
			return
		}
		var input requestBody
		if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: "invalid request"})
			return
		}
		input.VerifyParam = strings.TrimSpace(input.VerifyParam)
		if config.Enabled && input.VerifyParam == "" {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: "captcha verification is required"})
			return
		}
		payload := map[string]any{
			"source_id":    "autoclaw",
			"device_id":    deviceID,
			"navigate_uri": navigateURI,
		}
		if input.VerifyParam != "" {
			payload["ali_captcha_verify_param"] = input.VerifyParam
		}
		result, err := autoClawOAuthRequest(request.Context(), client, version, autoclawGoogleOAuthURLPath, payload)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: err.Error()})
			return
		}
		if result.code != 0 {
			writer.WriteHeader(http.StatusBadRequest)
			message := strings.TrimSpace(result.message)
			if message == "" {
				message = fmt.Sprintf("AutoClaw OAuth URL request failed with code %d", result.code)
			}
			_ = json.NewEncoder(writer).Encode(responseBody{Error: message})
			return
		}
		var data autoClawOAuthURLResponse
		if err := json.Unmarshal(result.data, &data); err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: "AutoClaw returned an invalid Google OAuth URL"})
			return
		}
		state, valid := autoClawGoogleOAuthState(data.OAuthURL)
		if !valid {
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(responseBody{Error: "AutoClaw returned an invalid Google OAuth URL"})
			return
		}
		oauthState.set(state)
		_ = json.NewEncoder(writer).Encode(responseBody{OK: true, OAuthURL: data.OAuthURL})
	}
}

func validAutoClawGoogleOAuthURL(value string) bool {
	_, valid := autoClawGoogleOAuthState(value)
	return valid
}

func autoClawGoogleOAuthState(value string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "accounts.google.com") {
		return "", false
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	return state, state != ""
}

func autoClawOAuthCallbackHandler(resultCh chan<- autoClawOAuthCallback, oauthState *autoClawOAuthState) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		query := request.URL.Query()
		result := autoClawOAuthCallback{
			code:  strings.TrimSpace(query.Get("code")),
			state: strings.TrimSpace(query.Get("state")),
			err:   strings.TrimSpace(query.Get("error")),
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !oauthState.matches(result.state) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(writer, "<h1>AutoClaw login rejected</h1><p>The OAuth state did not match this CCL login session.</p>")
			return
		}
		select {
		case resultCh <- result:
		default:
		}
		if result.err != "" || result.code == "" {
			_, _ = io.WriteString(writer, "<h1>AutoClaw login failed</h1><p>Return to the terminal for details.</p>")
			return
		}
		_, _ = io.WriteString(writer, "<h1>AutoClaw login received</h1><p>You can close this window and return to CCL.</p>")
	}
}

func exchangeAutoClawGoogleCode(ctx context.Context, client *http.Client, version, deviceID, navigateURI string, callback autoClawOAuthCallback) (autoClawOAuthLoginResponse, error) {
	payload := map[string]any{
		"source_id":    "autoclaw",
		"device_id":    deviceID,
		"code":         callback.code,
		"state":        callback.state,
		"navigate_uri": navigateURI,
	}
	result, err := autoClawOAuthRequest(ctx, client, version, autoclawGoogleOAuthLoginPath, payload)
	if err != nil {
		return autoClawOAuthLoginResponse{}, fmt.Errorf("exchange AutoClaw Google authorization: %w", err)
	}
	if result.code != 0 {
		return autoClawOAuthLoginResponse{}, fmt.Errorf("exchange AutoClaw Google authorization: code %d: %s", result.code, result.message)
	}
	var data autoClawOAuthLoginResponse
	if err := json.Unmarshal(result.data, &data); err != nil {
		return data, fmt.Errorf("decode AutoClaw Google authorization: %w", err)
	}
	data.AccessToken = stripBearerPrefix(data.AccessToken)
	data.RefreshToken = strings.TrimSpace(data.RefreshToken)
	if data.AccessToken == "" || data.RefreshToken == "" {
		return data, errors.New("AutoClaw Google authorization response is missing access_token or refresh_token")
	}
	return data, nil
}

func autoClawMetadataFromOAuth(data autoClawOAuthLoginResponse, deviceID, version string) map[string]any {
	metadata := map[string]any{
		"type":             ProviderAutoClaw,
		"access_token":     data.AccessToken,
		"refresh_token":    data.RefreshToken,
		"device_id":        deviceID,
		"base_url":         AutoClawOpenAIBaseURL(),
		"app_version":      version,
		"authenticated_at": time.Now().UTC().Format(time.RFC3339),
		"source":           "autoclaw_oauth",
	}
	if userID := autoClawJSONIdentifier(data.UserID); userID != "" {
		metadata["user_id"] = userID
	}
	if name := strings.TrimSpace(data.UserName); name != "" {
		metadata["name"] = name
	}
	if email := strings.TrimSpace(data.Email); email != "" {
		metadata["email"] = email
	}
	if expiresAt := autoclawJWTExpiry(data.AccessToken); !expiresAt.IsZero() {
		metadata["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	return metadata
}

func autoClawJSONIdentifier(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return strings.TrimSpace(number.String())
	}
	return ""
}

func autoClawOAuthPageHTML(encodedConfig, nonce string) string {
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>CCL · AutoClaw sign-in</title>
  <style>
    :root { color-scheme: light dark; font-family: -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #111318; color: #f4f5f7; }
    main { width: min(440px,calc(100vw - 48px)); padding: 32px; border: 1px solid #343944; border-radius: 16px; background: #1a1d24; box-shadow: 0 20px 70px #0008; }
    h1 { margin: 0 0 10px; font-size: 24px; } p { color: #b8bec9; line-height: 1.55; }
    button { width: 100%; margin-top: 18px; padding: 12px 16px; border: 0; border-radius: 10px; background: #7867ff; color: white; font-size: 16px; font-weight: 650; cursor: pointer; }
    button:disabled { opacity: .5; cursor: wait; } #status.error { color: #ff8b8b; } #captcha { position: relative; z-index: 2147483000; }
  </style>
</head>
<body>
<main>
  <h1>Sign in to AutoClaw</h1>
  <p>After the security check, you will continue to Google. Credentials return only to this CCL process and are stored under <code>~/.ccl/auth</code>.</p>
  <div id="captcha"></div>
  <button id="continue" type="button" disabled>Preparing security check…</button>
  <p id="status" role="status">Please wait.</p>
</main>
<script>
(() => {
  const config = JSON.parse(atob("` + encodedConfig + `"));
  const nonce = "` + nonce + `";
  const button = document.getElementById("continue");
  const status = document.getElementById("status");
  const setStatus = (message, error = false) => { status.textContent = message; status.className = error ? "error" : ""; };
  const requestOAuthURL = async (verifyParam = "") => {
    setStatus("Creating the Google sign-in session…");
    const response = await fetch("` + autoclawOAuthURLPath + `", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CCL-OAuth-Nonce": nonce },
      body: JSON.stringify({ ali_captcha_verify_param: verifyParam })
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok || !payload.ok || !payload.oauth_url) {
      throw new Error(payload.error || "Could not create the AutoClaw sign-in session");
    }
    setStatus("Continuing to Google…");
    window.setTimeout(() => window.location.assign(payload.oauth_url), 60);
    return { captchaResult: true, bizResult: true };
  };
  if (!config.enabled) {
    button.disabled = false;
    button.textContent = "Continue with Google";
    setStatus("Ready.");
    button.addEventListener("click", async () => {
      button.disabled = true;
      try { await requestOAuthURL(); } catch (error) { setStatus(error.message, true); button.disabled = false; }
    });
    return;
  }
  window.AliyunCaptchaConfig = { region: config.region, prefix: config.prefix };
  const script = document.createElement("script");
  script.src = "` + autoclawCaptchaScriptURL + `";
  script.async = true;
  script.onerror = () => setStatus("The security check could not load. Check your network and refresh this page.", true);
  script.onload = () => {
    if (typeof window.initAliyunCaptcha !== "function") {
      setStatus("The security check is unavailable. Refresh this page to try again.", true);
      return;
    }
    window.initAliyunCaptcha({
      SceneId: config.scene_id,
      mode: "popup",
      element: "#captcha",
      button: "#continue",
      captchaVerifyCallback: async (verifyParam) => {
        try { return await requestOAuthURL(verifyParam); }
        catch (error) { setStatus(error.message, true); return { captchaResult: false, bizResult: false }; }
      },
      onBizResultCallback: () => {},
      getInstance: () => {
        window.setTimeout(() => {
          button.disabled = false;
          button.textContent = "Verify and continue with Google";
          setStatus("Ready.");
        }, 2100);
      },
      slideStyle: { width: 360, height: 40 },
      language: "en",
      onError: () => setStatus("Security verification failed. Please try again.", true)
    });
  };
  document.head.appendChild(script);
})();
</script>
</body>
</html>`
}
