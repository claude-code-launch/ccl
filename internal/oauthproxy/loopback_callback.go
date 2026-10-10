package oauthproxy

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"time"
)

func writeOAuthCallbackPage(writer http.ResponseWriter, provider string, success bool) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	status, message := "complete", "You can close this tab and return to CCL."
	if !success {
		writer.WriteHeader(http.StatusBadRequest)
		status, message = "failed", "Return to CCL for details."
	}
	title := html.EscapeString(provider) + " login " + status
	_, _ = fmt.Fprintf(writer, "<!doctype html><meta charset=\"utf-8\"><title>%s</title><h2>%s</h2><p>%s</p>", title, title, message)
}

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
