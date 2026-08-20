package oauthproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	qoderOAuthTimeout      = 3 * time.Minute
	qoderHTTPTimeout       = 30 * time.Second
	qoderTokenExpiryBuffer = 5 * time.Minute
)

var (
	qoderLoginBaseURL   = "https://qoder.com/device/selectAccounts"
	qoderOpenAPIBaseURL = "https://openapi.qoder.sh"
	qoderCenterBaseURL  = "https://center.qoder.sh"
	qoderAPIBaseURL     = "https://api3.qoder.sh"
	qoderPollInterval   = 2 * time.Second
	qoderBrowserOpener  = openBrowser
)

type qoderDeviceToken struct {
	Token        string          `json:"token"`
	UserID       string          `json:"user_id"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
	ExpiresIn    int64           `json:"expires_in"`
}

type qoderUserInfo struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

func loginQoder(ctx context.Context, authDir string, opts LoginOptions) (LoginResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	verifier, challenge, err := qoderPKCE()
	if err != nil {
		return LoginResult{}, fmt.Errorf("create Qoder PKCE challenge: %w", err)
	}
	nonce := uuid.NewString()
	machineID := uuid.NewString()
	loginURL, err := url.Parse(qoderLoginBaseURL)
	if err != nil {
		return LoginResult{}, fmt.Errorf("build Qoder authorization URL: %w", err)
	}
	query := loginURL.Query()
	query.Set("challenge", challenge)
	query.Set("challenge_method", "S256")
	query.Set("machine_id", machineID)
	query.Set("nonce", nonce)
	loginURL.RawQuery = query.Encode()

	fmt.Printf("Open %s to authorize Qoder\n", loginURL.String())
	if !opts.NoBrowser {
		_ = qoderBrowserOpener(loginURL.String())
	}

	pollURL, err := url.Parse(strings.TrimRight(qoderOpenAPIBaseURL, "/") + "/api/v1/deviceToken/poll")
	if err != nil {
		return LoginResult{}, fmt.Errorf("build Qoder token poll URL: %w", err)
	}
	pollQuery := pollURL.Query()
	pollQuery.Set("nonce", nonce)
	pollQuery.Set("verifier", verifier)
	pollQuery.Set("challenge_method", "S256")
	pollURL.RawQuery = pollQuery.Encode()

	pollCtx, cancel := context.WithTimeout(ctx, qoderOAuthTimeout)
	defer cancel()
	client := &http.Client{Timeout: qoderHTTPTimeout}
	var token qoderDeviceToken
	for {
		if err := qoderWait(pollCtx, qoderPollInterval); err != nil {
			return LoginResult{}, fmt.Errorf("Qoder authorization: %w", err)
		}
		request, err := http.NewRequestWithContext(pollCtx, http.MethodGet, pollURL.String(), nil)
		if err != nil {
			return LoginResult{}, fmt.Errorf("poll Qoder authorization: %w", err)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "ccl")
		response, err := client.Do(request)
		if err != nil {
			return LoginResult{}, fmt.Errorf("poll Qoder authorization: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, qoderMaxErrorBytes))
		_ = response.Body.Close()
		if readErr != nil {
			return LoginResult{}, fmt.Errorf("poll Qoder authorization: read response: %w", readErr)
		}
		if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNotFound {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return LoginResult{}, fmt.Errorf("poll Qoder authorization: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		}
		if err := json.Unmarshal(body, &token); err != nil {
			return LoginResult{}, fmt.Errorf("decode Qoder device token: %w", err)
		}
		if strings.TrimSpace(token.Token) == "" || strings.TrimSpace(token.UserID) == "" {
			return LoginResult{}, fmt.Errorf("Qoder device token response is missing token or user_id")
		}
		break
	}

	profile := fetchQoderUserInfo(ctx, client, token.Token)
	name := strings.TrimSpace(profile.Name)
	if name == "" {
		name = strings.TrimSpace(profile.Username)
	}
	expiresAt := qoderExpiry(token.ExpiresAt, token.ExpiresIn)
	metadata := map[string]any{
		"type":          ProviderQoder,
		"access_token":  strings.TrimSpace(token.Token),
		"refresh_token": strings.TrimSpace(token.RefreshToken),
		"user_id":       strings.TrimSpace(token.UserID),
		"machine_id":    machineID,
		"expires_at":    expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if email := strings.TrimSpace(profile.Email); email != "" {
		metadata["email"] = email
	}
	if name != "" {
		metadata["name"] = name
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode Qoder credential: %w", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(authDir, ProviderQoder+"-"+credentialIdentity(metadata, raw)+".json")
	if err := writeCredentialAtomic(path, raw); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Provider: ProviderQoder, Backend: ProviderQoder, Path: path}, nil
}

func qoderPKCE() (verifier, challenge string, err error) {
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(random)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func qoderWait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func fetchQoderUserInfo(ctx context.Context, client *http.Client, accessToken string) qoderUserInfo {
	endpoint := strings.TrimRight(qoderOpenAPIBaseURL, "/") + "/api/v1/userinfo"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return qoderUserInfo{}
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccl")
	response, err := client.Do(request)
	if err != nil {
		return qoderUserInfo{}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return qoderUserInfo{}
	}
	var profile qoderUserInfo
	_ = json.NewDecoder(io.LimitReader(response.Body, qoderMaxErrorBytes)).Decode(&profile)
	return profile
}

func qoderExpiry(raw json.RawMessage, expiresIn int64) time.Time {
	if len(raw) > 0 && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil {
				return qoderBufferedExpiry(parsed)
			}
			if number, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
				return qoderBufferedExpiry(qoderUnixTime(number))
			}
		}
		var number int64
		if json.Unmarshal(raw, &number) == nil && number > 0 {
			return qoderBufferedExpiry(qoderUnixTime(number))
		}
	}
	if expiresIn > 0 {
		return qoderBufferedExpiry(time.Now().Add(time.Duration(expiresIn) * time.Second))
	}
	return qoderBufferedExpiry(time.Now().Add(30 * 24 * time.Hour))
}

func qoderUnixTime(value int64) time.Time {
	if value > 10_000_000_000 {
		return time.UnixMilli(value)
	}
	return time.Unix(value, 0)
}

func qoderBufferedExpiry(expiry time.Time) time.Time {
	if expiry.After(time.Now().Add(qoderTokenExpiryBuffer)) {
		return expiry.Add(-qoderTokenExpiryBuffer)
	}
	return expiry
}
