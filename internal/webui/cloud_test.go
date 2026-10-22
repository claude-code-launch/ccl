package webui

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCloudCCL stands in for `ccl cloud …`: it prints its arguments, fails
// a pull without --force, and a google-drive login waits for a pasted line.
const fakeCloudCCL = `echo "args: $*"
case "$2" in
  pull)
    case "$*" in *--force*) echo "Pulled." ;; *) echo "Error: local changes are not synchronized; use --force" >&2; exit 1 ;; esac ;;
  login)
    echo "Open https://accounts.example/o/oauth2/auth?state=s1 in a browser"
    if [ "$3" = google-drive ]; then read line; echo "got: $line"; fi ;;
esac
exit 0
`

func cloudServer(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := writeScript(t, t.TempDir(), "ccl", fakeCloudCCL)
	server, err := New(Config{Token: "secret", Workspace: t.TempDir(), StoreRoot: t.TempDir(), SelfBinary: bin})
	if err != nil {
		t.Fatal(err)
	}
	return testServerFrom(t, server)
}

func TestCloudOverviewAndCommands(t *testing.T) {
	base := cloudServer(t)
	status, body := doJSON(t, "GET", base+"/api/cloud", "secret", "")
	var overview struct {
		Remotes   []CloudRemote `json:"remotes"`
		Platforms []string      `json:"platforms"`
		Available bool          `json:"available"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &overview) != nil || len(overview.Remotes) != 0 || overview.Platforms[0] != "google-drive" || !overview.Available {
		t.Fatalf("overview = %d %s", status, body)
	}
	if status, _ := doJSON(t, "GET", base+"/api/cloud", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("no token = %d", status)
	}

	status, body = doJSON(t, "POST", base+"/api/cloud/push", "secret", `{"remote":"work","force":true}`)
	if status != http.StatusOK || !strings.Contains(string(body), "args: cloud push --to work --force") {
		t.Fatalf("push = %d %s", status, body)
	}
	// A refusal comes back as 409 with ccl's reason, so the page can offer --force.
	status, body = doJSON(t, "POST", base+"/api/cloud/pull", "secret", `{}`)
	if status != http.StatusConflict || !strings.Contains(string(body), `"error":"local changes are not synchronized; use --force"`) {
		t.Fatalf("pull = %d %s", status, body)
	}
	if status, body = doJSON(t, "POST", base+"/api/cloud/pull", "secret", `{"remote":"work","force":true}`); status != http.StatusOK || !strings.Contains(string(body), "args: cloud pull --from work --force") {
		t.Fatalf("forced pull = %d %s", status, body)
	}
	if status, body = doJSON(t, "POST", base+"/api/cloud/primary", "secret", `{"remote":"work"}`); status != http.StatusOK || !strings.Contains(string(body), "args: cloud remote use work") {
		t.Fatalf("primary = %d %s", status, body)
	}
	if status, body = doJSON(t, "POST", base+"/api/cloud/logout", "secret", `{"remote":"work"}`); status != http.StatusOK || !strings.Contains(string(body), "args: cloud logout work") {
		t.Fatalf("logout = %d %s", status, body)
	}
	for _, bad := range []string{`{"remote":"--all"}`, `{"remote":"a b"}`, `{}`} {
		if status, _ := doJSON(t, "POST", base+"/api/cloud/logout", "secret", bad); status != http.StatusBadRequest {
			t.Fatalf("logout %s = %d", bad, status)
		}
	}
}

func TestCloudLoginTakesAPastedAddress(t *testing.T) {
	// A development build has no Google OAuth client; the page refuses then.
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_ID", "test-client")
	t.Setenv("CCL_GOOGLE_OAUTH_CLIENT_SECRET", "test-secret")
	base := cloudServer(t)
	if status, _ := doJSON(t, "POST", base+"/api/cloud/login", "secret", `{"platform":"dropbox"}`); status != http.StatusBadRequest {
		t.Fatalf("unknown platform = %d", status)
	}
	status, body := doJSON(t, "POST", base+"/api/cloud/login", "secret", `{"platform":"google-drive","alias":"personal"}`)
	if status != http.StatusAccepted {
		t.Fatalf("login = %d %s", status, body)
	}
	var started struct{ ID string }
	_ = json.Unmarshal(body, &started)
	login := base + "/api/cloud/login/" + started.ID
	if status, _ := doJSON(t, "POST", login, "secret", `{"address":"a\nb"}`); status != http.StatusBadRequest {
		t.Fatalf("two lines = %d", status)
	}
	if status, body := doJSON(t, "POST", login, "secret", `{"address":"http://127.0.0.1:5555/oauth2/callback?state=s1&code=c1"}`); status != http.StatusNoContent {
		t.Fatalf("paste = %d %s", status, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = doJSON(t, "GET", login, "secret", "")
		var progress oauthLogin
		_ = json.Unmarshal(body, &progress)
		if progress.Done {
			output := strings.Join(progress.Output, "\n")
			if !progress.OK || !strings.Contains(output, "args: cloud login google-drive personal --no-browser") ||
				!strings.Contains(output, "got: http://127.0.0.1:5555/oauth2/callback?state=s1&code=c1") || progress.URLs[0] != "https://accounts.example/o/oauth2/auth?state=s1" {
				t.Fatalf("login = %s", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("login never finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, _ := doJSON(t, "POST", login, "secret", `{"address":"http://127.0.0.1/?code=x"}`); status != http.StatusConflict {
		t.Fatalf("paste after the login = %d", status)
	}
}

func TestCloudLoginFailureSaysWhy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := writeScript(t, t.TempDir(), "ccl", `echo "Logging in..."
echo "Error: the iCloud Drive folder is missing" >&2
exit 1
`)
	server, err := New(Config{Token: "secret", Workspace: t.TempDir(), StoreRoot: t.TempDir(), SelfBinary: bin})
	if err != nil {
		t.Fatal(err)
	}
	base := testServerFrom(t, server)
	// The page learns up front whether this build can log in to Google Drive.
	_, body := doJSON(t, "GET", base+"/api/cloud", "secret", "")
	if !strings.Contains(string(body), `"unavailable":{`) {
		t.Fatalf("overview = %s", body)
	}
	platform := "icloud"
	if runtime.GOOS != "darwin" {
		t.Skip("iCloud logins are macOS only")
	}
	_, body = doJSON(t, "POST", base+"/api/cloud/login", "secret", `{"platform":"`+platform+`"}`)
	var started struct{ ID string }
	_ = json.Unmarshal(body, &started)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = doJSON(t, "GET", base+"/api/cloud/login/"+started.ID, "secret", "")
		var progress oauthLogin
		_ = json.Unmarshal(body, &progress)
		if progress.Done {
			if progress.OK || progress.Error != "the iCloud Drive folder is missing" {
				t.Fatalf("login = %s", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("login never finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
