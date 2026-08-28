package cmd

import "testing"

func TestReleaseAssetName(t *testing.T) {
	name, err := releaseAssetName()
	if err != nil {
		t.Skipf("unsupported platform for self-update: %v", err)
	}
	valid := map[string]bool{
		"ccl-darwin-amd64":    true,
		"ccl-darwin-arm64":    true,
		"ccl-linux-amd64":     true,
		"ccl-linux-arm64":     true,
		"ccl-win32-x64.exe":   true,
		"ccl-win32-arm64.exe": true,
	}
	if !valid[name] {
		t.Errorf("releaseAssetName() = %q, want one of the release asset names", name)
	}
}

func TestReleaseDownloadURL(t *testing.T) {
	if got := releaseDownloadURL("v1.5.6", "ccl-darwin-arm64"); got != "https://github.com/claude-code-launch/ccl/releases/download/v1.5.6/ccl-darwin-arm64" {
		t.Errorf("versioned URL = %q", got)
	}
	if got := releaseDownloadURL("unknown", "ccl-linux-amd64"); got != "https://github.com/claude-code-launch/ccl/releases/latest/download/ccl-linux-amd64" {
		t.Errorf("latest URL = %q", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{5*1024*1024 + 512*1024, "5.5 MiB"},
	}
	for _, c := range cases {
		if got := humanSize(c.n); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
