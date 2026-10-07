package oauthproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// serveLoopbackCallback serves an OAuth flow's local callback page on
// listener. It returns a channel that receives an unexpected serve error (not
// a clean shutdown) and a stop function the caller defers; stop shuts the
// server down within two seconds. Every browser-based login shares it.
func serveLoopbackCallback(ctx context.Context, listener net.Listener, handler http.Handler) (<-chan error, func()) {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case errs <- err:
			default:
			}
		}
	}()
	return errs, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}
