package process

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func Command(path string, args ...string) *exec.Cmd {
	if ext := filepath.Ext(path); strings.EqualFold(ext, ".exe") || strings.EqualFold(ext, ".com") {
		return exec.Command(path, args...)
	}
	// npm's .cmd shims require cmd.exe. Its parser does not use the argument
	// quoting applied by os/exec, so construct its command line explicitly.
	// /s removes the outer quote pair; /d and /v:off disable AutoRun and
	// delayed !variable! expansion in the caller's shell configuration.
	words := []string{escapeCmdMeta(path)}
	for _, arg := range args {
		words = append(words, escapeCmdMeta(quoteWindowsArg(arg)))
	}
	line := `"` + strings.Join(words, " ") + `"`
	cmd := exec.Command("cmd.exe", "/d", "/s", "/v:off", "/c", line)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: syscall.EscapeArg(cmd.Path) + ` /d /s /v:off /c ` + line,
	}
	return cmd
}

// quoteWindowsArg quotes for the native program reached through the batch
// file. Backslashes before a quote (including the closing quote) are doubled.
func quoteWindowsArg(arg string) string {
	var out strings.Builder
	out.WriteByte('"')
	slashes := 0
	for _, char := range arg {
		if char == '\\' {
			slashes++
			continue
		}
		if char == '"' {
			out.WriteString(strings.Repeat(`\`, slashes*2+1))
		} else {
			out.WriteString(strings.Repeat(`\`, slashes))
		}
		out.WriteRune(char)
		slashes = 0
	}
	out.WriteString(strings.Repeat(`\`, slashes*2))
	out.WriteByte('"')
	return out.String()
}

func escapeCmdMeta(value string) string {
	var out strings.Builder
	for _, char := range value {
		if strings.ContainsRune("()[]%!^\"`<>&|;, *?\t", char) {
			out.WriteByte('^')
		}
		out.WriteRune(char)
	}
	return out.String()
}
