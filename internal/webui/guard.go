package webui

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard stops other web pages from driving ccl serve through the user's browser.
//
// Without a token the server trusts whoever reaches it, which is only safe if
// that is a page it served itself:
//   - Host must name the loopback interface. A DNS-rebinding page reaches
//     127.0.0.1 under its own host name, which this refuses.
//   - A request that changes state must not come from another site's page.
//     Browsers always send Origin on those.
//
// With a token, a forged request lacks the Authorization header, so neither
// check is needed; the LAN or container address the user chose is fine.
// Neither is needed on a Unix socket either: browsers cannot reach one, and
// its file mode admits only this user.
//
// Every JSON write must say it is JSON: a cross-site fetch with that content
// type needs a CORS preflight, which this server never grants.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" && !overUnixSocket(r) {
			if !loopbackName(hostOnly(r.Host)) {
				writeError(w, http.StatusForbidden, "ccl serve without a token only answers to 127.0.0.1, localhost or [::1]")
				return
			}
			if changesState(r.Method) && !sameMachineOrigin(r.Header.Get("Origin")) {
				writeError(w, http.StatusForbidden, "cross-site request refused")
				return
			}
		}
		if changesState(r.Method) && strings.HasPrefix(r.URL.Path, "/api/") && r.ContentLength != 0 && !jsonOrUpload(r) {
			writeError(w, http.StatusUnsupportedMediaType, "send Content-Type: application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func changesState(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// jsonOrUpload accepts JSON bodies everywhere and multipart bodies only on the
// upload routes.
func jsonOrUpload(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	if mediaType == "application/json" {
		return true
	}
	return mediaType == "multipart/form-data" && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/uploads")
}

// sameMachineOrigin accepts no Origin (curl, the CLI) and pages served from a
// loopback address on any port, which includes the Vite dev server.
func sameMachineOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return loopbackName(parsed.Hostname())
}

func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

func loopbackName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
