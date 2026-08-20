package oauthproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	KiroAuthModePortal    = "portal"
	KiroAuthModeBuilderID = "builder"

	kiroBuilderIDStartURL = "https://view.awsapps.com/start"
	kiroDefaultRegion     = "us-east-1"
	kiroBuilderProfileARN = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	kiroSocialProfileARN  = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
	kiroDeviceGrantType   = "urn:ietf:params:oauth:grant-type:device_code"
	kiroRefreshGrantType  = "refresh_token"
	kiroOAuthClientName   = "ccl"
	kiroOAuthRequestLimit = 30 * time.Second
	kiroSocialLoginLimit  = 10 * time.Minute
)

var kiroOIDCEndpoint = func(region string) string {
	return "https://oidc." + region + ".amazonaws.com"
}

var (
	kiroOAuthPollFloor = 5 * time.Second
	kiroAuthEndpoint   = func(region string) string {
		return "https://prod." + region + ".auth.desktop.kiro.dev"
	}
	kiroPortalSignInEndpoint = "https://app.kiro.dev/signin"
	kiroBrowserOpener        = openBrowser
)

var kiroCallbackPorts = []int{3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153}

type kiroRegisterClientResponse struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type kiroDeviceAuthorizationResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int64  `json:"expiresIn"`
	Interval                int64  `json:"interval"`
}

type kiroTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

type kiroOIDCError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type kiroSocialTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileARN   string `json:"profileArn"`
	IDP          string `json:"idp"`
	Provider     string `json:"provider"`
}

type kiroSocialCallback struct {
	code        string
	state       string
	loginOption string
	path        string
	err         error
}

func loginKiro(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.KiroAuthMode))
	if mode == "" {
		mode = KiroAuthModePortal
	}
	switch mode {
	case KiroAuthModePortal, "social", "web":
		return loginKiroSocial(ctx, authDir, opts)
	case KiroAuthModeBuilderID, "builder-id", "idc":
		return loginKiroBuilderID(ctx, authDir, opts)
	default:
		return LoginResult{}, fmt.Errorf("unsupported Kiro auth mode %q (use portal or builder)", opts.KiroAuthMode)
	}
}

func loginKiroBuilderID(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	client := &http.Client{Timeout: kiroOAuthRequestLimit}
	base := kiroOIDCEndpoint(kiroDefaultRegion)

	var registered kiroRegisterClientResponse
	err := kiroPostJSON(ctx, client, base+"/client/register", map[string]any{
		"clientName": kiroOAuthClientName,
		"clientType": "public",
		"scopes": []string{
			"codewhisperer:completions",
			"codewhisperer:analysis",
			"codewhisperer:conversations",
			"codewhisperer:transformations",
			"codewhisperer:taskassist",
		},
		"grantTypes": []string{kiroDeviceGrantType, kiroRefreshGrantType},
		"issuerUrl":  kiroBuilderIDStartURL,
	}, &registered)
	if err != nil {
		return LoginResult{}, fmt.Errorf("register Kiro OAuth client: %w", err)
	}
	if registered.ClientID == "" || registered.ClientSecret == "" {
		return LoginResult{}, fmt.Errorf("register Kiro OAuth client: incomplete response")
	}

	var device kiroDeviceAuthorizationResponse
	err = kiroPostJSON(ctx, client, base+"/device_authorization", map[string]any{
		"clientId":     registered.ClientID,
		"clientSecret": registered.ClientSecret,
		"startUrl":     kiroBuilderIDStartURL,
	}, &device)
	if err != nil {
		return LoginResult{}, fmt.Errorf("start Kiro device authorization: %w", err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return LoginResult{}, fmt.Errorf("start Kiro device authorization: incomplete response")
	}

	verificationURL := device.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = device.VerificationURI
	}
	fmt.Printf("Open %s and enter code %s\n", device.VerificationURI, device.UserCode)
	if !opts.NoBrowser {
		_ = kiroBrowserOpener(verificationURL)
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval < kiroOAuthPollFloor {
		interval = kiroOAuthPollFloor
	}
	expiresIn := time.Duration(device.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 10 * time.Minute
	}
	pollCtx, cancel := context.WithTimeout(ctx, expiresIn)
	defer cancel()

	var token kiroTokenResponse
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return LoginResult{}, fmt.Errorf("Kiro device authorization: %w", pollCtx.Err())
		case <-timer.C:
		}

		status, body, err := kiroPostJSONRaw(pollCtx, client, base+"/token", map[string]any{
			"clientId":     registered.ClientID,
			"clientSecret": registered.ClientSecret,
			"deviceCode":   device.DeviceCode,
			"grantType":    kiroDeviceGrantType,
		})
		if err != nil {
			return LoginResult{}, fmt.Errorf("poll Kiro device authorization: %w", err)
		}
		if status >= 200 && status < 300 {
			if err := json.Unmarshal(body, &token); err != nil {
				return LoginResult{}, fmt.Errorf("decode Kiro token response: %w", err)
			}
			break
		}
		var oauthErr kiroOIDCError
		_ = json.Unmarshal(body, &oauthErr)
		switch oauthErr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return LoginResult{}, fmt.Errorf("Kiro device authorization expired")
		case "access_denied":
			return LoginResult{}, fmt.Errorf("Kiro device authorization was denied")
		default:
			return LoginResult{}, fmt.Errorf("Kiro token endpoint returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
		}
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return LoginResult{}, fmt.Errorf("Kiro device authorization returned incomplete tokens")
	}

	metadata := map[string]any{
		"type":          ProviderKiro,
		"access_token":  token.AccessToken,
		"refresh_token": token.RefreshToken,
		"profile_arn":   kiroBuilderProfileARN,
		"auth_method":   "idc",
		"provider":      "BuilderId",
		"client_id":     registered.ClientID,
		"client_secret": registered.ClientSecret,
		"start_url":     kiroBuilderIDStartURL,
		"region":        kiroDefaultRegion,
		"auth_region":   kiroDefaultRegion,
		"api_region":    kiroDefaultRegion,
	}
	if token.ExpiresIn > 0 {
		metadata["expires_at"] = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Kiro credentials: %w", err)
	}
	raw = append(raw, '\n')
	fileName := ProviderKiro + "-" + credentialIdentity(metadata, raw) + ".json"
	path := filepath.Join(authDir, fileName)
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderKiro, Backend: ProviderKiro, Path: path}, nil
}

func loginKiroSocial(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	listener, err := listenKiroOAuthCallback(opts.CallbackPort)
	if err != nil {
		return LoginResult{}, err
	}
	defer listener.Close()

	state, err := randomKiroOAuthValue(32)
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate Kiro OAuth state: %w", err)
	}
	verifier, err := randomKiroOAuthValue(32)
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate Kiro PKCE verifier: %w", err)
	}
	challengeDigest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])
	redirectBase := "http://" + listener.Addr().String()

	signInURL, err := url.Parse(kiroPortalSignInEndpoint)
	if err != nil {
		return LoginResult{}, fmt.Errorf("parse Kiro Portal sign-in endpoint: %w", err)
	}
	query := signInURL.Query()
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("redirect_uri", redirectBase)
	query.Set("redirect_from", "KiroIDE")
	signInURL.RawQuery = query.Encode()

	callbacks := make(chan kiroSocialCallback, 1)
	var callbackOnce sync.Once
	deliver := func(callback kiroSocialCallback) {
		callbackOnce.Do(func() { callbacks <- callback })
	}
	mux := http.NewServeMux()
	handler := func(writer http.ResponseWriter, request *http.Request) {
		values := request.URL.Query()
		if oauthError := strings.TrimSpace(values.Get("error")); oauthError != "" {
			description := strings.TrimSpace(values.Get("error_description"))
			if description == "" {
				description = oauthError
			}
			deliver(kiroSocialCallback{err: fmt.Errorf("Kiro Portal authorization failed: %s", description)})
			writeKiroOAuthCallbackPage(writer, false)
			return
		}
		code := strings.TrimSpace(values.Get("code"))
		if code == "" {
			http.NotFound(writer, request)
			return
		}
		deliver(kiroSocialCallback{
			code:        code,
			state:       strings.TrimSpace(values.Get("state")),
			loginOption: strings.TrimSpace(values.Get("login_option")),
			path:        request.URL.Path,
		})
		writeKiroOAuthCallbackPage(writer, true)
	}
	mux.HandleFunc("/oauth/callback", handler)
	mux.HandleFunc("/signin/callback", handler)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			serveErrors <- serveErr
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	fmt.Printf("Open %s to sign in with your Kiro Google or GitHub account\n", signInURL.String())
	if !opts.NoBrowser {
		if err := kiroBrowserOpener(signInURL.String()); err != nil {
			fmt.Printf("Could not open a browser automatically; open the URL above manually: %v\n", err)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, kiroSocialLoginLimit)
	defer cancel()
	var callback kiroSocialCallback
	select {
	case <-waitCtx.Done():
		return LoginResult{}, fmt.Errorf("Kiro Portal authorization: %w", waitCtx.Err())
	case serveErr := <-serveErrors:
		return LoginResult{}, fmt.Errorf("serve Kiro OAuth callback: %w", serveErr)
	case callback = <-callbacks:
	}
	if callback.err != nil {
		return LoginResult{}, callback.err
	}
	if callback.state == "" || callback.state != state {
		return LoginResult{}, fmt.Errorf("Kiro Portal authorization returned an invalid state")
	}

	callbackPath := callback.path
	if callbackPath != "/oauth/callback" && callbackPath != "/signin/callback" {
		return LoginResult{}, fmt.Errorf("Kiro Portal authorization returned an invalid callback path")
	}
	tokenRedirectURI := redirectBase + callbackPath
	if callback.loginOption != "" {
		tokenRedirectURI += "?login_option=" + url.QueryEscape(callback.loginOption)
	}

	client := &http.Client{Timeout: kiroOAuthRequestLimit}
	token, err := exchangeKiroSocialCode(waitCtx, client, callback.code, verifier, tokenRedirectURI)
	if err != nil {
		return LoginResult{}, err
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return LoginResult{}, fmt.Errorf("Kiro Portal authorization returned incomplete tokens")
	}

	idp := normalizeKiroPortalIDP(callback.loginOption)
	if idp == "" {
		idp = normalizeKiroPortalIDP(token.IDP)
	}
	if idp == "" {
		idp = normalizeKiroPortalIDP(token.Provider)
	}
	metadata := map[string]any{
		"type":          ProviderKiro,
		"access_token":  token.AccessToken,
		"refresh_token": token.RefreshToken,
		"profile_arn":   kiroSocialProfileARN,
		"auth_method":   "social",
		"region":        kiroDefaultRegion,
		"auth_region":   kiroDefaultRegion,
		"api_region":    kiroDefaultRegion,
	}
	if token.ProfileARN != "" {
		metadata["profile_arn"] = token.ProfileARN
	}
	if idp != "" {
		metadata["provider"] = idp
	}
	if token.ExpiresAt != "" {
		metadata["expires_at"] = token.ExpiresAt
	} else if token.ExpiresIn > 0 {
		metadata["expires_at"] = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	hydrateKiroPortalLoginMetadata(waitCtx, client, metadata)

	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Kiro credentials: %w", err)
	}
	raw = append(raw, '\n')
	fileName := ProviderKiro + "-" + credentialIdentity(metadata, raw) + ".json"
	path := filepath.Join(authDir, fileName)
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderKiro, Backend: ProviderKiro, Path: path}, nil
}

func listenKiroOAuthCallback(callbackPort int) (net.Listener, error) {
	if callbackPort < 0 || callbackPort > 65535 {
		return nil, fmt.Errorf("Kiro OAuth callback port must be between 1 and 65535")
	}
	if callbackPort > 0 {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(callbackPort)))
		if err != nil {
			return nil, fmt.Errorf("listen on Kiro OAuth callback port %d: %w", callbackPort, err)
		}
		return listener, nil
	}
	var lastErr error
	for _, port := range kiroCallbackPorts {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return listener, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("listen for Kiro OAuth callback: all supported ports are unavailable: %w", lastErr)
}

func randomKiroOAuthValue(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeKiroOAuthCallbackPage(writer http.ResponseWriter, success bool) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if success {
		_, _ = io.WriteString(writer, "<!doctype html><meta charset=\"utf-8\"><title>Kiro login complete</title><h2>Kiro login complete</h2><p>You can close this tab and return to CCL.</p>")
		return
	}
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(writer, "<!doctype html><meta charset=\"utf-8\"><title>Kiro login failed</title><h2>Kiro login failed</h2><p>Return to CCL for details.</p>")
}

func exchangeKiroSocialCode(ctx context.Context, client *http.Client, code, verifier, redirectURI string) (kiroSocialTokenResponse, error) {
	payload := map[string]any{
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return kiroSocialTokenResponse{}, err
	}
	endpoint := kiroAuthEndpoint(kiroDefaultRegion) + "/oauth/token"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return kiroSocialTokenResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "KiroIDE-"+kiroIDEVersion)
	response, err := client.Do(request)
	if err != nil {
		return kiroSocialTokenResponse{}, fmt.Errorf("exchange Kiro Portal authorization code: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return kiroSocialTokenResponse{}, fmt.Errorf("read Kiro Portal token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return kiroSocialTokenResponse{}, fmt.Errorf("exchange Kiro Portal authorization code: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token kiroSocialTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return kiroSocialTokenResponse{}, fmt.Errorf("decode Kiro Portal token response: %w", err)
	}
	return token, nil
}

func normalizeKiroPortalIDP(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(normalized, "google"):
		return "Google"
	case strings.Contains(normalized, "github"):
		return "Github"
	case strings.Contains(normalized, "builder"):
		return "BuilderId"
	case strings.Contains(normalized, "idc"), strings.Contains(normalized, "enterprise"):
		return "AWSIdC"
	default:
		return ""
	}
}

func hydrateKiroPortalLoginMetadata(ctx context.Context, client *http.Client, metadata map[string]any) {
	credential := kiroCredential{
		metadata:     metadata,
		accessToken:  metadataString(metadata, "access_token"),
		refreshToken: metadataString(metadata, "refresh_token"),
		profileARN:   metadataString(metadata, "profile_arn"),
		authMethod:   metadataString(metadata, "auth_method"),
		provider:     metadataString(metadata, "provider"),
	}
	catalog := newKiroModelCatalog(nil)
	session, attempted, err := catalog.bootstrapPortalSession(ctx, &kiroService{client: client}, credential)
	if !attempted || err != nil {
		LogWarnf("kiro portal login metadata hydration attempted=%t error=%v", attempted, err)
		return
	}
	if session.csrfToken != "" {
		metadata["csrf_token"] = session.csrfToken
	}
	if session.userID != "" {
		metadata["user_id"] = session.userID
	}
	if session.profileARN != "" {
		metadata["profile_arn"] = session.profileARN
	}
	if idp := kiroPortalIDP(&session); idp != "" {
		metadata["provider"] = idp
	}
	metadata["visitor_id"] = kiroPortalVisitorID(&session)
}

func kiroPostJSON(ctx context.Context, client *http.Client, endpoint string, body any, target any) error {
	status, raw, err := kiroPostJSONRaw(ctx, client, endpoint, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func kiroPostJSONRaw(ctx context.Context, client *http.Client, endpoint string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, responseBody, nil
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}
