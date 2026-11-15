package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPath is the longest Unix socket path every platform accepts
// (sun_path is 104 bytes on macOS, including the terminating NUL).
const maxSocketPath = 103

// DefaultSocketPath is where `ccl serve --socket` listens when it is given no
// path: next to the conversations, in a directory only this user can open.
func DefaultSocketPath() (string, error) {
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(root), "ccl.sock"), nil
}

// ListenUnix listens on a Unix socket at path that only this user can
// connect to. A socket left behind by a server that died is replaced; one
// another server still answers on is refused, as is any other file.
func ListenUnix(path string) (net.Listener, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is longer than %d bytes; pick a shorter one", path, maxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&fs.ModeSocket == 0:
		return nil, fmt.Errorf("%s exists and is not a socket", path)
	case err == nil:
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another server is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Connecting needs write permission on the socket. Closing the listener
	// removes the file.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict socket: %w", err)
	}
	return listener, nil
}

// overUnixSocket reports whether r arrived on a Unix socket, which no
// browser page can reach and only this user can connect to.
func overUnixSocket(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && addr.Network() == "unix"
}
