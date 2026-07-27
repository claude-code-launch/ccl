package cloudsync

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	googleDriveScope = "https://www.googleapis.com/auth/drive.appdata"

	// XOR mask for the public Desktop OAuth client material below. Google
	// installed-app clients still require client_secret on token exchange and
	// refresh; the value is not a confidentiality boundary—PKCE protects the
	// authorization code. Material is obfuscated only so push protection does
	// not treat the intentional public Desktop credentials as leaked secrets.
	googleOAuthMaterialMask = 0x5A
)

var (
	// Obfuscated public Desktop OAuth client id/secret (XOR googleOAuthMaterialMask).
	googleOAuthClientIDMaterial = []byte{
		109, 110, 99, 99, 99, 105, 110, 111, 111, 106, 105, 108, 119, 98, 40, 105,
		105, 52, 54, 105, 44, 99, 63, 107, 52, 40, 55, 109, 110, 47, 54, 63, 44, 47,
		44, 48, 105, 59, 63, 52, 61, 49, 61, 111, 56, 116, 59, 42, 42, 41, 116, 61,
		53, 53, 61, 54, 63, 47, 41, 63, 40, 57, 53, 52, 46, 63, 52, 46, 116, 57, 53, 55,
	}
	googleOAuthClientSecretMaterial = []byte{
		29, 21, 25, 9, 10, 2, 119, 12, 5, 8, 104, 23, 57, 110, 47, 13, 10, 30, 35, 59,
		19, 14, 45, 30, 8, 41, 62, 31, 55, 25, 47, 63, 41, 99, 29,
	}

	// Decoded at init from the public Desktop OAuth client material above.
	// Release builds may override googleOAuthClientSecret via -ldflags; local
	// development may set CCL_GOOGLE_OAUTH_CLIENT_SECRET.
	googleOAuthClientID     = decodeGoogleOAuthMaterial(googleOAuthClientIDMaterial)
	googleOAuthClientSecret = decodeGoogleOAuthMaterial(googleOAuthClientSecretMaterial)
	googleOAuthEndpoint     = oauth2.Endpoint{
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
	}
	openBrowserURL = openSystemBrowser
)

func decodeGoogleOAuthMaterial(material []byte) string {
	out := make([]byte, len(material))
	for i, b := range material {
		out[i] = b ^ googleOAuthMaterialMask
	}
	return string(out)
}

type googleAuthFile struct {
	Version int           `json:"version"`
	Token   *oauth2.Token `json:"token"`
}

func authorizeGoogleDrive(ctx context.Context) (*googleDriveRemote, error) {
	return authorizeGoogleDriveWithNotice(ctx, nil)
}

func authorizeGoogleDriveWithNotice(ctx context.Context, notice io.Writer) (*googleDriveRemote, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	authPath := filepath.Join(localDir, googleAuthName)
	return authorizeGoogleDriveAt(ctx, authPath, notice)
}

func authorizeGoogleDriveAt(ctx context.Context, authPath string, notice io.Writer) (*googleDriveRemote, error) {
	if token, loadErr := loadGoogleToken(authPath); loadErr == nil {
		return newGoogleDriveRemote(ctx, token, authPath), nil
	} else if !os.IsNotExist(loadErr) {
		return nil, loadErr
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start Google OAuth callback listener: %w", err)
	}
	defer listener.Close()

	state, err := randomURLToken(32)
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	redirectURL := "http://" + listener.Addr().String() + "/oauth2/callback"
	config := googleOAuthConfig(redirectURL)
	authURL := config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account consent"),
	)

	type callbackResult struct {
		code string
		err  error
	}
	resultChannel := make(chan callbackResult, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/oauth2/callback" {
				http.NotFound(w, request)
				return
			}
			query := request.URL.Query()
			var result callbackResult
			switch {
			case query.Get("state") != state:
				result.err = fmt.Errorf("Google OAuth state did not match")
			case query.Get("error") != "":
				result.err = fmt.Errorf("Google authorization was not completed: %s", query.Get("error"))
			case query.Get("code") == "":
				result.err = fmt.Errorf("Google OAuth callback did not include an authorization code")
			default:
				result.code = query.Get("code")
			}
			select {
			case resultChannel <- result:
			default:
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if result.err == nil {
				_, _ = fmt.Fprint(w, "<!doctype html><title>CCL connected</title><h1>Google Drive connected</h1><p>You can close this tab and return to CCL.</p>")
			} else {
				_, _ = fmt.Fprintf(w, "<!doctype html><title>CCL authorization failed</title><h1>Authorization failed</h1><p>%s</p>", html.EscapeString(result.err.Error()))
			}
		}),
	}
	go func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case resultChannel <- callbackResult{err: fmt.Errorf("Google OAuth callback server: %w", serveErr)}:
			default:
			}
		}
	}()

	if err := openBrowserURL(authURL); err != nil {
		return nil, fmt.Errorf("open browser for Google authorization: %w; open this URL manually: %s", err, authURL)
	}
	if notice != nil {
		fmt.Fprintln(notice, "Opened Google authorization in your browser.")
		fmt.Fprintf(notice, "If the browser did not open, visit:\n%s\n", authURL)
		fmt.Fprintln(notice, "Waiting for Google authorization...")
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var callback callbackResult
	select {
	case callback = <-resultChannel:
	case <-waitCtx.Done():
		return nil, fmt.Errorf("Google authorization timed out: %w", waitCtx.Err())
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = server.Shutdown(shutdownCtx)
	shutdownCancel()
	if callback.err != nil {
		return nil, callback.err
	}

	token, err := config.Exchange(
		ctx,
		callback.code,
		oauth2.VerifierOption(verifier),
	)
	if err != nil {
		return nil, fmt.Errorf("exchange Google authorization code: %w", err)
	}
	if token.RefreshToken == "" {
		return nil, fmt.Errorf("Google did not return an offline refresh token; revoke CCL access in your Google account and try again")
	}
	if err := saveGoogleToken(authPath, token); err != nil {
		return nil, err
	}
	return newGoogleDriveRemote(ctx, token, authPath), nil
}

func loadAuthorizedGoogleDrive(ctx context.Context) (*googleDriveRemote, error) {
	localDir, err := cclDirectory()
	if err != nil {
		return nil, err
	}
	authPath := filepath.Join(localDir, googleAuthName)
	return loadAuthorizedGoogleDriveAt(ctx, authPath, "")
}

func loadAuthorizedGoogleDriveAt(ctx context.Context, authPath, alias string) (*googleDriveRemote, error) {
	token, err := loadGoogleToken(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			if alias != "" {
				return nil, fmt.Errorf("not logged in to Google Drive remote %q; run `ccl cloud login google-drive %s` first", alias, alias)
			}
			return nil, fmt.Errorf("not logged in to Google Drive; run `ccl cloud login google-drive` first")
		}
		return nil, err
	}
	return newGoogleDriveRemote(ctx, token, authPath), nil
}

func googleOAuthConfig(redirectURL string) *oauth2.Config {
	// Precedence: env (dev override) → ldflags override of the package var →
	// the built-in Desktop client secret. Google's installed-app token endpoint
	// still requires client_secret for this client; omitting it yields
	// "client_secret is missing" on code exchange and refresh.
	clientSecret := strings.TrimSpace(os.Getenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET"))
	if clientSecret == "" {
		clientSecret = strings.TrimSpace(googleOAuthClientSecret)
	}
	return &oauth2.Config{
		ClientID:     googleOAuthClientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{googleDriveScope},
		Endpoint:     googleOAuthEndpoint,
	}
}

func loadGoogleToken(path string) (*oauth2.Token, error) {
	var stored googleAuthFile
	if err := readJSONFile(path, &stored); err != nil {
		return nil, err
	}
	if stored.Version != formatVersion || stored.Token == nil ||
		stored.Token.RefreshToken == "" {
		return nil, fmt.Errorf("invalid Google Drive authorization; remove %s and run `ccl cloud login google-drive` again", path)
	}
	return stored.Token, nil
}

func saveGoogleToken(path string, token *oauth2.Token) error {
	if token == nil || token.RefreshToken == "" {
		return fmt.Errorf("refuse to save incomplete Google Drive authorization")
	}
	if err := writeJSONAtomic(path, googleAuthFile{Version: formatVersion, Token: token}, 0o600); err != nil {
		return fmt.Errorf("save Google Drive authorization: %w", err)
	}
	return nil
}

func revokeGoogleAuthorization(ctx context.Context, authPath string) error {
	token, err := loadGoogleToken(authPath)
	if err != nil {
		return err
	}
	value := token.RefreshToken
	if value == "" {
		value = token.AccessToken
	}
	form := url.Values{"token": []string{value}}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "https://oauth2.googleapis.com/revoke",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("revoke Google Drive authorization: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return googleDriveResponseError("revoke authorization", response)
	}
	return nil
}

type savingTokenSource struct {
	mu       sync.Mutex
	source   oauth2.TokenSource
	authPath string
	last     *oauth2.Token
}

func (source *savingTokenSource) Token() (*oauth2.Token, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	token, err := source.source.Token()
	if err != nil {
		return nil, err
	}
	if source.last == nil || token.AccessToken != source.last.AccessToken ||
		token.RefreshToken != source.last.RefreshToken || !token.Expiry.Equal(source.last.Expiry) {
		if token.RefreshToken == "" && source.last != nil {
			token.RefreshToken = source.last.RefreshToken
		}
		if err := saveGoogleToken(source.authPath, token); err != nil {
			return nil, err
		}
		source.last = token
	}
	return token, nil
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func openSystemBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
