package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, url string, files map[string][]byte) (int, []Attachment, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, content := range files {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(content)
	}
	writer.Close()
	request, _ := http.NewRequest("POST", url, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var saved []Attachment
	_ = json.Unmarshal(raw, &saved)
	return response.StatusCode, saved, string(raw)
}

func TestUploadsDetectTypeAndServeSafely(t *testing.T) {
	httpServer := testServer(t, Config{Token: "secret"})
	status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, body)
	}
	var meta Meta
	_ = json.Unmarshal(body, &meta)
	base := httpServer.URL + "/api/sessions/" + meta.ID + "/uploads"

	status, saved, raw := upload(t, base, map[string][]byte{
		"shot.png":     pngBytes(t),
		"notes.txt":    []byte("hello"),
		"evil.svg":     []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"fake.png":     []byte("not really a png"),
		"../escape.md": []byte("# x"),
	})
	if status != http.StatusCreated || len(saved) != 5 {
		t.Fatalf("upload = %d: %s", status, raw)
	}
	byName := map[string]Attachment{}
	for _, attachment := range saved {
		byName[attachment.Name] = attachment
	}
	if a := byName["shot.png"]; !a.Image || a.Type != "image/png" {
		t.Fatalf("png = %+v", a)
	}
	// The bytes decide what is an image, not the name the browser sent.
	if a := byName["fake.png"]; a.Image {
		t.Fatalf("a text file named .png became an image: %+v", a)
	}
	if a := byName["evil.svg"]; a.Image {
		t.Fatalf("svg is sent inline: %+v", a)
	}
	if _, ok := byName["escape.md"]; !ok {
		t.Fatalf("a path in the file name was not reduced to its base: %+v", saved)
	}

	get := func(id string) *http.Response {
		response, err := http.Get(base + "/" + id + "?token=secret")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}
	if response := get(byName["shot.png"].ID); response.StatusCode != 200 || response.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("png served as %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	svg := get(byName["evil.svg"].ID)
	if svg.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(svg.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("svg served as %q / %q", svg.Header.Get("Content-Type"), svg.Header.Get("Content-Disposition"))
	}
	if !strings.Contains(svg.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("attachments are served without a sandbox policy")
	}
	if response := get("..%2Fmeta.json"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal = %d", response.StatusCode)
	}
	// Downloads need the token like every other session route.
	response, err := http.Get(base + "/" + byName["shot.png"].ID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("download without a token = %d", response.StatusCode)
	}
}

func TestMessageWithAttachmentsUsesStreamJSON(t *testing.T) {
	var (
		mu   sync.Mutex
		seen LaunchSpec
	)
	launch := func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		mu.Lock()
		seen = spec
		mu.Unlock()
		return helperPrepare(t, `{"type":"result","subtype":"success","is_error":false}`, false)(ctx, spec)
	}
	httpServer := testServer(t, Config{Token: "secret", Launch: launch})
	_, body := doJSON(t, "POST", httpServer.URL+"/api/sessions", "secret", `{}`)
	var meta Meta
	_ = json.Unmarshal(body, &meta)
	_, saved, raw := upload(t, httpServer.URL+"/api/sessions/"+meta.ID+"/uploads", map[string][]byte{
		"shot.png": pngBytes(t),
		"data.csv": []byte("a,b\n1,2\n"),
	})
	if len(saved) != 2 {
		t.Fatalf("upload: %s", raw)
	}
	ids := `["` + saved[0].ID + `","` + saved[1].ID + `"]`

	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"","attachments":["nope"]}`); status != http.StatusBadRequest {
		t.Fatalf("unknown attachment = %d: %s", status, body)
	}
	// A message may be attachments alone.
	if status, body := doJSON(t, "POST", httpServer.URL+"/api/sessions/"+meta.ID+"/messages", "secret", `{"text":"","attachments":`+ids+`}`); status != http.StatusAccepted {
		t.Fatalf("message = %d: %s", status, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := seen
		mu.Unlock()
		if len(got.Attachments) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("launcher saw %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The stored user message lists the attachments for the page.
	_, events := doJSON(t, "GET", httpServer.URL+"/api/sessions/"+meta.ID+"/events?token=secret", "", "")
	if !strings.Contains(string(events), `"attachments":[`) {
		t.Fatalf("user event lacks attachments: %s", events)
	}
	// The conversation is titled after the first file when there is no text.
	_, single := doJSON(t, "GET", httpServer.URL+"/api/sessions/"+meta.ID, "secret", "")
	if !strings.Contains(string(single), saved[0].Name) {
		t.Fatalf("title not taken from the attachment: %s", single)
	}
	// The turn may still be writing; TempDir cleanup must wait for it.
	waitIdle(t, httpServer.URL, meta.ID)
}

func TestStdinMessageCarriesImagesAndFilePaths(t *testing.T) {
	dir := t.TempDir()
	imagePath := dir + "/a.png"
	data := pngBytes(t)
	if err := os.WriteFile(imagePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{
		Prompt: "what is this?",
		Attachments: []LaunchAttachment{
			{Path: imagePath, Name: "a.png", MediaType: "image/png", Image: true},
			{Path: dir + "/report.pdf", Name: "report.pdf", MediaType: "application/pdf"},
		},
	}
	args := buildArgs(spec)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--input-format stream-json") || args[len(args)-1] == spec.Prompt {
		t.Fatalf("args = %v", args)
	}
	line, err := stdinMessage(spec)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatalf("stdin line is not JSON: %v\n%s", err, line)
	}
	content := message.Message.Content
	if message.Type != "user" || message.Message.Role != "user" || len(content) != 2 {
		t.Fatalf("message = %s", line)
	}
	if content[0].Type != "image" || content[0].Source.MediaType != "image/png" || content[0].Source.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image block = %+v", content[0])
	}
	if content[1].Type != "text" || !strings.Contains(content[1].Text, "what is this?") || !strings.Contains(content[1].Text, dir+"/report.pdf") {
		t.Fatalf("text block = %q", content[1].Text)
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Fatal("stream-json input must end with a newline")
	}
}

func TestTranslatorInitCapabilities(t *testing.T) {
	var got InitPayload
	newTranslator().Handle([]byte(`{"type":"system","subtype":"init","session_id":"s","slash_commands":["review","compact"],"mcp_servers":[{"name":"github","status":"connected"}]}`),
		func(kind string, payload any) { got = payload.(InitPayload) })
	if len(got.SlashCommands) != 2 || got.SlashCommands[0] != "review" || len(got.MCPServers) != 1 || got.MCPServers[0].Status != "connected" {
		t.Fatalf("init = %+v", got)
	}
}

func TestSanitizeFileName(t *testing.T) {
	for input, want := range map[string]string{
		"../../etc/passwd": "passwd",
		`C:\Users\a\b.txt`: "b.txt",
		"a:b*c?.png":       "a_b_c_.png",
		"...":              "file",
		"截图 1.png":         "截图 1.png",
	} {
		if got := sanitizeFileName(input); got != want {
			t.Fatalf("sanitizeFileName(%q) = %q, want %q", input, got, want)
		}
	}
}
