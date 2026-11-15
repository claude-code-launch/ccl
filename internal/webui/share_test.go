package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestShareLinkIsReadOnlyAndRevocable(t *testing.T) {
	base := testServer(t, Config{Token: "secret"}).URL
	var session SessionView
	_, body := doJSON(t, "POST", base+"/api/sessions", "secret", `{"title":"shared one"}`)
	_ = json.Unmarshal(body, &session)
	if status, _ := doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/share", "", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("share without the server token = %d", status)
	}
	status, body := doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/share", "secret", `{}`)
	var link struct{ Token, Path string }
	if status != http.StatusOK || json.Unmarshal(body, &link) != nil || len(link.Token) != 64 || link.Path != "/share/"+link.Token {
		t.Fatalf("share = %d %s", status, body)
	}
	// Sharing again keeps the same link.
	_, again := doJSON(t, "POST", base+"/api/sessions/"+session.ID+"/share", "secret", `{}`)
	if !strings.Contains(string(again), link.Token) {
		t.Fatalf("second share = %s", again)
	}

	// The link needs no server token, and reveals the folder name only.
	status, body = doJSON(t, "GET", base+"/api/share/"+link.Token, "", "")
	var shared SharedSession
	if status != http.StatusOK || json.Unmarshal(body, &shared) != nil || shared.Title != "shared one" || strings.Contains(shared.Workspace, "/") {
		t.Fatalf("shared = %d %s", status, body)
	}
	for _, path := range []string{"/api/share/" + strings.Repeat("0", 64), "/api/share/short", "/api/share/" + link.Token + "/events"} {
		if status, _ := doJSON(t, "GET", base+path, "", ""); status != http.StatusNotFound {
			t.Fatalf("%s = %d", path, status)
		}
	}
	if status, _ := doJSON(t, "POST", base+"/api/share/"+link.Token, "", `{}`); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a share link = %d", status)
	}
	response, err := http.Get(base + link.Path)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(page), "<div id=\"root\">") || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("share page = %d %q", response.StatusCode, response.Header.Get("Referrer-Policy"))
	}

	if status, _ := doJSON(t, "DELETE", base+"/api/sessions/"+session.ID+"/share", "secret", ""); status != http.StatusNoContent {
		t.Fatalf("revoke = %d", status)
	}
	if status, _ := doJSON(t, "GET", base+"/api/share/"+link.Token, "", ""); status != http.StatusNotFound {
		t.Fatalf("revoked link = %d", status)
	}
}
