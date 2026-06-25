//go:build darwin || linux

package locale

import (
	"os"
	"strings"
)

// systemLanguage detects the system language on Unix-like platforms.
// Priority: LC_ALL > LC_MESSAGES > LANG
func systemLanguage() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(key); v != "" {
			v = strings.ToLower(v)
			switch {
			case strings.HasPrefix(v, "zh"):
				return "zh-CN"
			default:
				return "en-US"
			}
		}
	}
	return "en-US"
}
