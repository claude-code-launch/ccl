package locale

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"zh":     "zh-CN",
		"zh-CN":  "zh-CN",
		"zh_cn":  "zh-CN",
		"zh-TW":  "zh-TW",
		"zh-HK":  "zh-TW",
		"en":     "en-US",
		"en-US":  "en-US",
		"EN_GB":  "en-US",
		"fr-FR":  "en-US",
		"":       "en-US",
		"  ZH  ": "zh-CN",
	}
	for input, want := range tests {
		if got := canonicalize(input); got != want {
			t.Errorf("canonicalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSetLanguageAndT(t *testing.T) {
	// LanguageManager is process-global; keep this test sequential.
	original := Current()
	t.Cleanup(func() { SetLanguage(original) })

	if got := SetLanguage("zh"); got != "zh-CN" {
		t.Fatalf("SetLanguage(zh) = %q, want zh-CN", got)
	}
	if !IsChinese() {
		t.Fatal("IsChinese() = false after SetLanguage(zh)")
	}
	if got := T("中文", "English"); got != "中文" {
		t.Fatalf("T chinese = %q", got)
	}
	if got := Tf("你好 %s", "Hello %s", "ccl"); got != "你好 ccl" {
		t.Fatalf("Tf chinese = %q", got)
	}

	if got := SetLanguage("en"); got != "en-US" {
		t.Fatalf("SetLanguage(en) = %q, want en-US", got)
	}
	if IsChinese() {
		t.Fatal("IsChinese() = true after SetLanguage(en)")
	}
	if got := T("中文", "English"); got != "English" {
		t.Fatalf("T english = %q", got)
	}
	if got := Tf("你好 %s", "Hello %s", "ccl"); got != "Hello ccl" {
		t.Fatalf("Tf english = %q", got)
	}
}

func TestReadConfigLang(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ccl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ccl", "config.yaml"), []byte("lang: zh-CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readConfigLang(); got != "zh-CN" {
		t.Fatalf("readConfigLang = %q, want zh-CN", got)
	}
}
