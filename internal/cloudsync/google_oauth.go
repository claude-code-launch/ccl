package cloudsync

import (
	"bufio"
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
	"strings"
	"sync"
	"time"

	"github.com/claude-code-launch/ccl/internal/browser"
	"golang.org/x/oauth2"
)

const googleDriveScope = "https://www.googleapis.com/auth/drive.appdata"

var (
	// The Desktop OAuth client of ccl's Google Cloud project. Release builds
	// inject it with -ldflags (GOOGLE_OAUTH_CLIENT_ID/_SECRET); local
	// development may set CCL_GOOGLE_OAUTH_CLIENT_ID/_SECRET. Installed-app
	// clients are public: Google still requires the secret on token requests,
	// but PKCE protects the authorization code. appDataFolder belongs to the
	// Cloud project, so every machine must use a client of the same project.
	googleOAuthClientID     = ""
	googleOAuthClientSecret = ""
	googleOAuthEndpoint     = oauth2.Endpoint{
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
	}
	openBrowserURL = browser.Open
)

func googleDesktopClient() (string, string) {
	id := strings.TrimSpace(os.Getenv("CCL_GOOGLE_OAUTH_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET"))
	if id == "" {
		id = strings.TrimSpace(googleOAuthClientID)
	}
	if secret == "" {
		secret = strings.TrimSpace(googleOAuthClientSecret)
	}
	return id, secret
}

func requireGoogleDesktopClient() error {
	if id, _ := googleDesktopClient(); id == "" {
		return fmt.Errorf("this ccl build has no Google OAuth client; use a release build or set CCL_GOOGLE_OAUTH_CLIENT_ID and CCL_GOOGLE_OAUTH_CLIENT_SECRET")
	}
	return nil
}

type googleAuthFile struct {
	Version int           `json:"version"`
	Token   *oauth2.Token `json:"token"`
}

type pastedRedirectKey struct{}

// WithPastedRedirect makes Google Drive login in ctx print the authorization
// URL instead of opening a browser, and read the address the browser was
// redirected to from in. Hosts without a browser (SSH, Cloud Shell) need this:
// Google's device flow rejects the drive.appdata scope, and the 127.0.0.1
// redirect only reaches ccl from a browser on the same machine.
func WithPastedRedirect(ctx context.Context, in io.Reader) context.Context {
	return context.WithValue(ctx, pastedRedirectKey{}, in)
}

func pastedRedirectInput(ctx context.Context) io.Reader {
	in, _ := ctx.Value(pastedRedirectKey{}).(io.Reader)
	return in
}

type callbackResult struct {
	code string
	err  error
}

func checkGoogleCallback(query url.Values, state string) callbackResult {
	switch {
	case query.Get("state") != state:
		return callbackResult{err: fmt.Errorf("Google OAuth state did not match")}
	case query.Get("error") != "":
		return callbackResult{err: fmt.Errorf("Google authorization was not completed: %s", query.Get("error"))}
	case query.Get("code") == "":
		return callbackResult{err: fmt.Errorf("Google OAuth callback did not include an authorization code")}
	}
	return callbackResult{code: query.Get("code")}
}

// readPastedGoogleRedirect reads lines until one is a redirected address
// (anything carrying a code or error parameter) and reports its result.
func readPastedGoogleRedirect(in io.Reader, state string, notice io.Writer, results chan<- callbackResult) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if index := strings.Index(line, "?"); index >= 0 {
			line = line[index+1:]
		}
		line, _, _ = strings.Cut(line, "#")
		query, err := url.ParseQuery(line)
		if err != nil || (query.Get("code") == "" && query.Get("error") == "") {
			if notice != nil {
				fmt.Fprint(notice, "That is not the redirected address; paste the whole 127.0.0.1 address: ")
			}
			continue
		}
		select {
		case results <- checkGoogleCallback(query, state):
		default:
		}
		return
	}
}

func authorizeGoogleDriveAt(ctx context.Context, authPath string, notice io.Writer) (*googleDriveRemote, error) {
	if token, loadErr := loadGoogleToken(authPath); loadErr == nil {
		return newGoogleDriveRemote(ctx, token, authPath), nil
	} else if !os.IsNotExist(loadErr) {
		return nil, loadErr
	}
	if err := requireGoogleDesktopClient(); err != nil {
		return nil, err
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

	resultChannel := make(chan callbackResult, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/oauth2/callback" {
				http.NotFound(w, request)
				return
			}
			result := checkGoogleCallback(request.URL.Query(), state)
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

	timeout := 5 * time.Minute
	if pasted := pastedRedirectInput(ctx); pasted != nil {
		// The browser may run on another machine, so its redirect to
		// 127.0.0.1 never reaches this listener (unless the port is
		// forwarded); the user pastes the redirected address instead.
		if notice != nil {
			fmt.Fprintf(notice, "Open this URL in a browser on any device and approve access:\n\n%s\n\n", authURL)
			fmt.Fprintln(notice, "The browser then opens a 127.0.0.1 address that fails to load. Copy that whole")
			fmt.Fprint(notice, "address from the address bar and paste it here: ")
		}
		go readPastedGoogleRedirect(pasted, state, notice, resultChannel)
		timeout = 15 * time.Minute
	} else {
		if err := openBrowserURL(authURL); err != nil {
			return nil, fmt.Errorf("open browser for Google authorization: %w; open this URL manually: %s", err, authURL)
		}
		if notice != nil {
			fmt.Fprintln(notice, "Opened Google authorization in your browser.")
			fmt.Fprintf(notice, "If the browser did not open, visit:\n%s\n", authURL)
			fmt.Fprintln(notice, "Waiting for Google authorization...")
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
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
	// Google's installed-app token endpoint still requires client_secret;
	// omitting it yields "client_secret is missing" on exchange and refresh.
	clientID, clientSecret := googleDesktopClient()
	return &oauth2.Config{
		ClientID:     clientID,
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
