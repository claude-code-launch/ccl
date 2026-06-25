//go:build windows

package locale

import (
	"golang.org/x/sys/windows"
)

// systemLanguage detects the system language on Windows via GetUserDefaultUILanguage.
// LANG_CHINESE = 0x04, sublanguage bits tell us zh-CN vs zh-TW.
func systemLanguage() string {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GetUserDefaultUILanguage")

	langID, _, _ := proc.Call()

	// The primary language ID is the low 10 bits of the language ID.
	primaryLang := uint16(langID) & 0x3ff

	if primaryLang == 0x04 {
		// Check sublanguage: SUBLANG_CHINESE_TRADITIONAL makes it zh-TW
		subLang := uint16(langID>>10) & 0x3f
		if subLang == 0x01 || subLang == 0x02 { // SUBLANG_CHINESE_TRADITIONAL or MACAU
			return "zh-TW"
		}
		return "zh-CN"
	}
	return "en-US"
}
