package webui

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
)

// NormalizeBasePath returns the URL prefix ccl serve is mounted under: "" for
// the root, otherwise the path without a trailing slash ("/chat/" is "/chat").
// Only plain, unencoded segments are accepted, so a proxy, ServeMux and the
// browser cannot read the same prefix differently.
func NormalizeBasePath(value string) (string, error) {
	if value == "" || value == "/" {
		return "", nil
	}
	invalid := errors.New("base path must be an absolute path of plain segments, such as /chat (no host, query, encoding, empty, '.' or '..' segments)")
	if !strings.HasPrefix(value, "/") {
		return "", invalid
	}
	value = strings.TrimSuffix(value, "/")
	for _, segment := range strings.Split(value[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", invalid
		}
		for _, char := range segment {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~", char)) {
				return "", invalid
			}
		}
	}
	// The root /health stays the container probe under a prefix.
	if value == "/health" {
		return "", errors.New("base path /health is reserved for the health check")
	}
	return value, nil
}

// stripBasePath hands next the path it would see at the root. The request is
// cloned rather than the ResponseWriter wrapped, so streams keep flushing.
func stripBasePath(basePath string, next http.Handler) http.Handler {
	if basePath == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !withinBasePath(r.URL.Path, basePath) || r.URL.RawPath != "" && !withinBasePath(r.URL.RawPath, basePath) {
			http.NotFound(w, r)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path = strings.TrimPrefix(r.URL.Path, basePath)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		if r.URL.RawPath != "" {
			r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, basePath)
			if r.URL.RawPath == "" {
				r.URL.RawPath = "/"
			}
		}
		next.ServeHTTP(w, r)
	})
}

func withinBasePath(path, basePath string) bool {
	return basePath == "" || path == basePath || strings.HasPrefix(path, basePath+"/")
}

// indexBaseSlot is the one tag of the built page the server fills in: the
// page resolves its assets and API calls against it.
const indexBaseSlot = `<base href="/" data-ccl-base>`

// renderIndex fills the base slot and touches nothing else, compiled scripts
// included.
func renderIndex(index []byte, basePath string) ([]byte, error) {
	if bytes.Count(index, []byte(indexBaseSlot)) != 1 {
		return nil, errors.New("the web UI's index.html must contain exactly one " + indexBaseSlot)
	}
	return bytes.Replace(index, []byte(indexBaseSlot), []byte(`<base href="`+basePath+`/" data-ccl-base>`), 1), nil
}
