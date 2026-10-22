package webui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Attachment is a file the user added to a message. It lives in the
// conversation's uploads directory, so deleting the conversation deletes it.
type Attachment struct {
	// ID is the stored file name: a random prefix and the sanitized original
	// name, so the name needs no separate record.
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Image marks a file sent to the model as an image block. Other files are
	// handed to Claude Code by path.
	Image bool `json:"image"`
}

const (
	// MaxImageBytes is the largest image sent inline; the Messages API refuses
	// bigger ones. A larger image is still attached, by path.
	MaxImageBytes = 5 << 20
	// MaxUploadBytes bounds one attached file.
	MaxUploadBytes = 30 << 20
	// maxUploadRequest bounds one upload request carrying several files.
	maxUploadRequest = 100 << 20
)

// inlineImageTypes are the image formats Claude accepts as image blocks. SVG is
// deliberately absent: it is a document that can carry script.
var inlineImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

var attachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{12}-[^/\\]{1,120}$`)

func (s *Session) uploadsDir() string { return filepath.Join(s.dir, "uploads") }

// SaveUpload stores one uploaded file.
func (s *Session) SaveUpload(name string, content io.Reader) (Attachment, error) {
	dir := s.uploadsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, fmt.Errorf("create uploads directory: %w", err)
	}
	prefix := make([]byte, 6)
	if _, err := rand.Read(prefix); err != nil {
		return Attachment{}, err
	}
	id := hex.EncodeToString(prefix) + "-" + sanitizeFileName(name)
	path := filepath.Join(dir, id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Attachment{}, fmt.Errorf("store upload: %w", err)
	}
	written, err := io.Copy(file, io.LimitReader(content, MaxUploadBytes+1))
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && written > MaxUploadBytes {
		err = fmt.Errorf("%s is larger than %d MB", name, MaxUploadBytes>>20)
	}
	if err != nil {
		_ = os.Remove(path)
		return Attachment{}, err
	}
	return s.Attachment(id)
}

// Attachment describes a stored upload.
func (s *Session) Attachment(id string) (Attachment, error) {
	if !attachmentIDPattern.MatchString(id) || id != filepath.Base(id) {
		return Attachment{}, ErrNotFound
	}
	path := filepath.Join(s.uploadsDir(), id)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Attachment{}, ErrNotFound
	}
	mediaType := detectType(path, id)
	return Attachment{
		ID:    id,
		Name:  id[13:],
		Type:  mediaType,
		Size:  info.Size(),
		Image: inlineImageTypes[mediaType] && info.Size() <= MaxImageBytes,
	}, nil
}

// AttachmentPath is where Claude Code reads an attachment.
func (s *Session) AttachmentPath(id string) string { return filepath.Join(s.uploadsDir(), id) }

// detectType trusts the content over the name, which the browser chose.
func detectType(path, name string) string {
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		head := make([]byte, 512)
		n, _ := io.ReadFull(file, head)
		if sniffed := http.DetectContentType(head[:n]); sniffed != "application/octet-stream" {
			mediaType, _, _ := mime.ParseMediaType(sniffed)
			if inlineImageTypes[mediaType] || !strings.HasPrefix(sniffed, "text/plain") {
				return mediaType
			}
		}
	}
	if byName := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); byName != "" {
		mediaType, _, _ := mime.ParseMediaType(byName)
		// The extension never makes a file an inline image: only its bytes do.
		if !inlineImageTypes[mediaType] {
			return mediaType
		}
	}
	return "application/octet-stream"
}

// sanitizeFileName keeps a recognizable name that is safe as one path
// element on every platform.
func sanitizeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.Trim(strings.TrimSpace(b.String()), ".")
	if clean == "" {
		clean = "file"
	}
	runes := []rune(clean)
	if len(runes) > 100 {
		ext := filepath.Ext(clean)
		if len([]rune(ext)) > 20 {
			ext = ""
		}
		clean = string(runes[:100-len([]rune(ext))]) + ext
	}
	return clean
}

func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request, session *Session) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequest)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "no file in the upload")
		return
	}
	saved := make([]Attachment, 0, len(files))
	for _, header := range files {
		if header.Size > MaxUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is larger than %d MB", header.Filename, MaxUploadBytes>>20))
			return
		}
		content, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		attachment, err := session.SaveUpload(header.Filename, content)
		content.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		saved = append(saved, attachment)
	}
	writeJSON(w, http.StatusCreated, saved)
}

// serveUpload returns an attachment to the page. Only inline image types are
// shown inline; anything else downloads, and nothing runs in this origin.
func (s *Server) serveUpload(w http.ResponseWriter, session *Session, id string) {
	attachment, err := session.Attachment(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment not found")
		return
	}
	serveSafely(w, session.AttachmentPath(id), attachment.Name, attachment.Type, "private, max-age=31536000, immutable")
}

// resolveAttachments checks the ids a message refers to.
func resolveAttachments(session *Session, ids []string) ([]Attachment, error) {
	attachments := make([]Attachment, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		attachment, err := session.Attachment(id)
		if err != nil {
			return nil, fmt.Errorf("attachment %q not found", id)
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}
