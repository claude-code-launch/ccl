// Package locale provides cross-platform language detection and translation.
//
// Detection priority:
//  1. CCL_LANG environment variable
//  2. ~/.ccl/config.yaml lang field
//  3. OS-level language setting (via environment on Unix, Win32 API on Windows)
package locale

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// LanguageManager manages the current language and provides translations.
type LanguageManager struct {
	mu   sync.RWMutex
	lang string // full locale code, e.g. "zh-CN", "en-US"
}

var (
	instance *LanguageManager
	once     sync.Once
)

func getInstance() *LanguageManager {
	once.Do(func() {
		inst := &LanguageManager{lang: "en-US"}
		inst.init()
		instance = inst
	})
	return instance
}

func (lm *LanguageManager) init() {
	// 1. CCL_LANG env var (highest priority)
	if v := os.Getenv("CCL_LANG"); v != "" {
		lm.lang = canonicalize(v)
		return
	}

	// 2. Config file (medium priority)
	if v := readConfigLang(); v != "" {
		lm.lang = canonicalize(v)
		return
	}

	// 3. OS-level detection (lowest priority)
	lm.lang = canonicalize(systemLanguage())
}

// readConfigLang reads the lang field from config.yaml.
func readConfigLang() string {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".ccl", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Lang string `yaml:"lang"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.Lang
}

// canonicalize normalizes a language code to a full locale (zh-CN, en-US, etc.)
func canonicalize(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	switch {
	case strings.HasPrefix(code, "zh"):
		if strings.Contains(code, "tw") || strings.Contains(code, "hk") || strings.Contains(code, "mo") {
			return "zh-TW"
		}
		return "zh-CN"
	default:
		return "en-US"
	}
}

// --- Public API ---

// SetLanguage overrides the language at runtime and returns the canonical form.
func SetLanguage(code string) string {
	mgr := getInstance()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.lang = canonicalize(code)
	return mgr.lang
}

// Current returns the current full locale code (e.g. "zh-CN", "en-US").
func Current() string {
	mgr := getInstance()
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return mgr.lang
}

// IsChinese returns true when the current language is a Chinese variant.
func IsChinese() bool {
	return strings.HasPrefix(Current(), "zh")
}

// T returns the Chinese or English string based on the current language.
// Call: T("中文文本", "English text")
func T(zh, en string) string {
	if IsChinese() {
		return zh
	}
	return en
}

// Tf is like T but supports fmt.Sprintf formatting.
// Call: Tf("中文 %s 模板", "English %s template", arg)
func Tf(zhPattern, enPattern string, args ...any) string {
	pattern := T(zhPattern, enPattern)
	return fmt.Sprintf(pattern, args...)
}
