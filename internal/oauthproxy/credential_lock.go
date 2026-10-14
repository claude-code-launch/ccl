package oauthproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

func lockOAuthCredential(ctx context.Context, path string) (func(), error) {
	// Cloud restore replaces auth/ as a directory. Keep the lock alongside
	// that directory so restoring credentials cannot create a second inode
	// while another process still holds the original refresh lock.
	directory := filepath.Dir(path) + ".refresh-locks"
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	return fsutil.AcquireFileLock(ctx, filepath.Join(directory, filepath.Base(path)+".lock"))
}

type rejectedOAuthTokenKey struct{}

// Carry the token the upstream rejected across a refresh callback. The
// authorizer re-reads under its file lock and reuses a newer token, preventing
// delayed 401s from refreshing the same account again.
func withRejectedOAuthToken(ctx context.Context, token string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rejectedOAuthTokenKey{}, token)
}

func oauthTokenStillRejected(ctx context.Context, current string) bool {
	if ctx == nil {
		return true
	}
	rejected, ok := ctx.Value(rejectedOAuthTokenKey{}).(string)
	return !ok || rejected == "" || current == rejected
}

func rejectedResponseContext(ctx context.Context, response *http.Response) context.Context {
	if response == nil || response.Request == nil {
		return ctx
	}
	return withRejectedOAuthToken(ctx, strings.TrimPrefix(response.Request.Header.Get("Authorization"), "Bearer "))
}
