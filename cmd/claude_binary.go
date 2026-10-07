package cmd

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// claudeBinaryPath is var so tests can point the env-var check at a fixture.
var claudeBinaryPath = func() (string, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// claudeReadsEnvVar reports whether the installed Claude Code mentions name,
// by searching the binary's bytes — nothing is executed. known is false when
// there is no binary to inspect, so callers stay silent rather than guess.
// Every variable Claude Code reads appears in it as a string literal, so a
// name that is absent cannot have an effect.
func claudeReadsEnvVar(name string) (read, known bool) {
	path, err := claudeBinaryPath()
	if err != nil || name == "" {
		return false, false
	}
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > 2<<30 {
		return false, false
	}
	needle := []byte(name)
	const chunk = 4 << 20
	buf := make([]byte, chunk+len(needle))
	carry := 0
	for {
		n, err := f.Read(buf[carry : carry+chunk])
		window := buf[:carry+n]
		if bytes.Contains(window, needle) {
			return true, true
		}
		if err == io.EOF {
			return false, true
		}
		if err != nil {
			return false, false
		}
		// Keep the tail so a name split across reads is still found.
		carry = min(len(needle)-1, len(window))
		copy(buf, window[len(window)-carry:])
	}
}
