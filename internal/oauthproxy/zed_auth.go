package oauthproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	zedDefaultServerURL = "https://zed.dev"
	zedDefaultCloudURL  = "https://cloud.zed.dev"
	// zedClientVersion is the Zed release CCL identifies as. Zed's cloud rejects
	// clients below its minimum supported version, so keep this near the current
	// stable release.
	zedClientVersion       = "1.22.0"
	zedSignInPath          = "/native_app_signin"
	zedSignInSucceededPath = "/native_app_signin_succeeded"
	zedLoginTimeout        = 5 * time.Minute
)

var (
	zedServerBaseURL = zedDefaultServerURL
	zedCloudBaseURL  = zedDefaultCloudURL
	// zedBrowserOpener is a variable so tests can complete the sign-in without a
	// real browser.
	zedBrowserOpener = openBrowser
)

// zedUserAgent reproduces the identity string Zed's HTTP client sends.
func zedUserAgent() string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return fmt.Sprintf("Zed/%s (%s; %s)", zedClientVersion, osName, arch)
}

// newZedKeypair returns a fresh RSA key and its public half in the encoding
// Zed's sign-in page expects: a PKCS#1 DER public key in padded base64url. The
// server encrypts the access token to this key, so the token never crosses the
// loopback redirect in the clear.
func newZedKeypair() (*rsa.PrivateKey, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}
	return key, base64.URLEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey)), nil
}

// decryptZedAccessToken decrypts the base64url ciphertext Zed redirects with.
// Current servers use OAEP-SHA256; older ones used PKCS#1 v1.5, which Zed's own
// client still accepts as a fallback.
func decryptZedAccessToken(key *rsa.PrivateKey, encrypted string) (string, error) {
	encrypted = strings.TrimSpace(encrypted)
	raw, err := base64.URLEncoding.DecodeString(encrypted)
	if err != nil {
		var rawErr error
		raw, rawErr = base64.RawURLEncoding.DecodeString(strings.TrimRight(encrypted, "="))
		if rawErr != nil {
			return "", fmt.Errorf("decode Zed access token: %w", err)
		}
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), nil, key, raw, nil)
	if err != nil {
		var legacyErr error
		plain, legacyErr = rsa.DecryptPKCS1v15(nil, key, raw)
		if legacyErr != nil {
			return "", fmt.Errorf("decrypt Zed access token: %w", err)
		}
	}
	if !utf8.Valid(plain) || len(plain) == 0 {
		return "", fmt.Errorf("decrypt Zed access token: result is not a valid token")
	}
	return string(plain), nil
}

type zedLoginCallback struct {
	userID      string
	accessToken string
}

// zedCallbackHandler receives Zed's redirect to the loopback port. Requests
// that do not carry both parameters (favicon fetches, port scanners) are
// rejected without consuming the pending sign-in.
func zedCallbackHandler(results chan<- zedLoginCallback, successURL string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		callback := zedLoginCallback{
			userID:      strings.TrimSpace(query.Get("user_id")),
			accessToken: strings.TrimSpace(query.Get("access_token")),
		}
		if request.Method != http.MethodGet || callback.userID == "" || callback.accessToken == "" {
			http.Error(writer, "waiting for the Zed sign-in redirect", http.StatusBadRequest)
			return
		}
		select {
		case results <- callback:
		default:
		}
		http.Redirect(writer, request, successURL, http.StatusFound)
	})
}

// loginZed runs Zed's native-app sign-in: the browser signs in at zed.dev and
// redirects the encrypted access token to a loopback port owned by this
// process. The long-lived token is validated against the cloud API and stored
// under ~/.ccl/auth; LLM tokens are minted from it at runtime.
func loginZed(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	privateKey, publicKey, err := newZedKeypair()
	if err != nil {
		return LoginResult{}, fmt.Errorf("create Zed sign-in key: %w", err)
	}

	address := "127.0.0.1:0"
	if opts.CallbackPort > 0 {
		address = net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.CallbackPort))
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return LoginResult{}, fmt.Errorf("listen for Zed sign-in callback on %s: %w", address, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	systemID := uuidString()
	results := make(chan zedLoginCallback, 1)
	server := &http.Server{
		Handler:           zedCallbackHandler(results, strings.TrimRight(zedServerBaseURL, "/")+zedSignInSucceededPath),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	loginURL := strings.TrimRight(zedServerBaseURL, "/") + zedSignInPath + "?" + url.Values{
		"native_app_port":       {strconv.Itoa(port)},
		"native_app_public_key": {publicKey},
		"system_id":             {systemID},
	}.Encode()
	fmt.Printf("Open %s to sign in to Zed\n", loginURL)
	if !opts.NoBrowser {
		_ = zedBrowserOpener(loginURL)
	}
	fmt.Println("Waiting for Zed sign-in...")

	timeout := time.NewTimer(zedLoginTimeout)
	defer timeout.Stop()
	var callback zedLoginCallback
	select {
	case callback = <-results:
	case <-timeout.C:
		return LoginResult{}, fmt.Errorf("Zed sign-in timed out")
	case <-ctx.Done():
		return LoginResult{}, fmt.Errorf("Zed sign-in: %w", ctx.Err())
	}

	userID, err := strconv.ParseUint(callback.userID, 10, 64)
	if err != nil {
		return LoginResult{}, fmt.Errorf("Zed sign-in returned an invalid user id %q", callback.userID)
	}
	accessToken, err := decryptZedAccessToken(privateKey, callback.accessToken)
	if err != nil {
		return LoginResult{}, err
	}

	auth := zedUserAuth{userID: strconv.FormatUint(userID, 10), accessToken: accessToken, systemID: systemID}
	client := &http.Client{Timeout: 30 * time.Second}
	profile, profileErr := fetchZedProfile(ctx, client, auth)
	var httpErr *zedHTTPError
	if errors.As(profileErr, &httpErr) && httpErr.status == http.StatusUnauthorized {
		return LoginResult{}, fmt.Errorf("Zed rejected the new sign-in: %w", profileErr)
	}
	if profileErr != nil {
		// The token itself is valid even when the profile fetch hit a transient
		// failure, and losing it would force another browser round trip.
		fmt.Printf("Warning: could not verify the Zed account yet: %v\n", profileErr)
	}

	metadata := map[string]any{
		"type":         ProviderZed,
		"user_id":      auth.userID,
		"access_token": accessToken,
		"system_id":    systemID,
		"timestamp":    time.Now().UnixMilli(),
	}
	if profile != nil {
		if login := profile.login(); login != "" {
			metadata["login"] = login
		}
		if name := strings.TrimSpace(profile.User.Name); name != "" {
			metadata["name"] = name
		}
		if organizationID := profile.organizationID(); organizationID != "" {
			metadata["organization_id"] = organizationID
		}
		if plan := profile.planName(); plan != "" {
			metadata["plan"] = plan
		}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Zed credential: %w", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(authDir, ProviderZed+"-"+credentialIdentity(metadata, raw)+".json")
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}

	fmt.Println("Zed authentication successful")
	if profile != nil {
		fmt.Printf("Account: %s (plan: %s)\n", zedFirstNonEmpty(profile.login(), auth.userID), zedFirstNonEmpty(profile.planName(), "unknown"))
		if profile.planName() == "zed_free" {
			fmt.Println("Note: the Zed Free plan does not include hosted models; subscribe or start a trial at https://zed.dev/account")
		}
	}
	return LoginResult{Provider: ProviderZed, Backend: ProviderZed, Path: path}, nil
}

func zedFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
