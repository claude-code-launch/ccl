package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/provider"
)

func TestParseLangArgRejectsUnknownCodesInsteadOfDefaultingToEnglish(t *testing.T) {
	// `ccl lang zn` (a typo for zh) used to save English and report success.
	for _, arg := range []string{"zn", "xyz", "de", "zh-XX", "1", "-"} {
		if code, err := parseLangArg(arg); err == nil {
			t.Fatalf("parseLangArg(%q) = %q, want an error", arg, code)
		}
	}
}

func TestParseLangArgAcceptsTheSpellingsUsersType(t *testing.T) {
	for arg, want := range map[string]string{
		"zh": "zh-CN", "ZH": "zh-CN", " cn ": "zh-CN", "zh_CN": "zh-CN", "chinese": "zh-CN",
		"zh-TW": "zh-TW", "hk": "zh-TW",
		"en": "en-US", "EN-US": "en-US", "english": "en-US",
	} {
		got, err := parseLangArg(arg)
		if err != nil {
			t.Fatalf("parseLangArg(%q): %v", arg, err)
		}
		if got != want {
			t.Fatalf("parseLangArg(%q) = %q, want %q", arg, got, want)
		}
	}
}

func TestParseLangChoiceTreatsNoAnswerAsKeepCurrent(t *testing.T) {
	// An unanswered prompt is EOF, which used to select English and rewrite
	// config.yaml as a side effect of merely asking what the language was.
	for _, answer := range []string{"", "  ", "\n"} {
		if _, err := parseLangChoice(answer); !errors.Is(err, errLangNeedsChoice) {
			t.Fatalf("parseLangChoice(%q): err = %v, want errLangNeedsChoice", answer, err)
		}
	}

	if _, err := parseLangChoice("3"); err == nil || errors.Is(err, errLangNeedsChoice) {
		t.Fatalf("parseLangChoice(\"3\"): err = %v, want an invalid-choice error", err)
	}

	for answer, want := range map[string]string{"1": "zh-CN", "2": "en-US"} {
		got, err := parseLangChoice(answer)
		if err != nil {
			t.Fatalf("parseLangChoice(%q): %v", answer, err)
		}
		if got != want {
			t.Fatalf("parseLangChoice(%q) = %q, want %q", answer, got, want)
		}
	}
}

func TestBareLangWithoutATerminalLeavesTheConfigAlone(t *testing.T) {
	// Under `go test` stdin is not a terminal — the same situation as
	// `ccl lang < /dev/null` or a CI step. Nothing may be written.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCL_LANG", "")
	if err := config.Save(&provider.Config{Lang: "zh-CN", Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	buf := new(bytes.Buffer)
	langCmd.SetOut(buf)
	t.Cleanup(func() { langCmd.SetOut(nil) })
	if err := langCmd.RunE(langCmd, nil); err != nil {
		t.Fatalf("bare ccl lang: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Lang != "zh-CN" {
		t.Fatalf("lang = %q, want the seeded zh-CN to survive an unanswered prompt", cfg.Lang)
	}
	if !strings.Contains(buf.String(), "unchanged") && !strings.Contains(buf.String(), "未改动") {
		t.Fatalf("output does not say the config was left alone:\n%s", buf.String())
	}
}

func TestLangReportsThatCclLangEnvWins(t *testing.T) {
	// locale resolves CCL_LANG ahead of config.yaml, so without this warning the
	// command claims to have switched while the next run reads the environment.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCL_LANG", "en")
	if err := config.Save(&provider.Config{Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	buf := new(bytes.Buffer)
	if err := applyLang(buf, "zh-CN"); err != nil {
		t.Fatalf("applyLang: %v", err)
	}
	if !strings.Contains(buf.String(), "CCL_LANG") {
		t.Fatalf("no warning that CCL_LANG overrides the saved language:\n%s", buf.String())
	}

	// The value is still saved, so the setting applies once the export is gone.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Lang != "zh-CN" {
		t.Fatalf("lang = %q, want zh-CN saved despite the override", cfg.Lang)
	}
}

func TestApplyLangIsQuietWhenNothingChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCL_LANG", "")
	if err := config.Save(&provider.Config{Lang: "en-US", Providers: map[string]provider.Provider{}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	buf := new(bytes.Buffer)
	if err := applyLang(buf, "en-US"); err != nil {
		t.Fatalf("applyLang: %v", err)
	}
	if cfg, err := config.Load(); err != nil || cfg.Lang != "en-US" {
		t.Fatalf("lang = %v (err %v), want en-US", cfg.Lang, err)
	}
}
