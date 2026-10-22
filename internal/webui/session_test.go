package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestStoreCreateAppendAndReopen(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "first", Provider: "default", Workspace: "/tmp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.ID() == "" {
		t.Fatal("Create assigned no id")
	}
	for i := 0; i < 3; i++ {
		event, err := session.Append(EventToken, TokenPayload{Text: "x"})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
	}

	// A second store over the same directory is a restart: the sequence must
	// continue rather than overwrite history.
	reopened, err := NewStore(store.Root())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	again, err := reopened.Open(session.ID())
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	if got := again.LastSeq(); got != 3 {
		t.Fatalf("LastSeq after restart = %d, want 3", got)
	}
	event, err := again.Append(EventResult, ResultPayload{})
	if err != nil {
		t.Fatalf("Append after restart: %v", err)
	}
	if event.Seq != 4 {
		t.Fatalf("seq after restart = %d, want 4", event.Seq)
	}
}

func TestSessionEventsSince(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"a", "b", "c"} {
		if _, err := session.Append(kind, map[string]int{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := session.Events(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 2 || events[1].Seq != 3 {
		t.Fatalf("Events(1) = %+v", events)
	}
	all, err := session.Events(0)
	if err != nil || len(all) != 3 {
		t.Fatalf("Events(0) = %d events, err %v", len(all), err)
	}
	if string(all[0].Payload) != `{"n":1}` {
		t.Fatalf("payload = %s", all[0].Payload)
	}
}

func TestSessionOnAppendFires(t *testing.T) {
	store := newTestStore(t)
	var seen []string
	store.OnAppend = func(id string) { seen = append(seen, id) }
	session, err := store.Create(Meta{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(EventUser, map[string]string{"text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != session.ID() {
		t.Fatalf("OnAppend calls = %v", seen)
	}
}

func TestStoreListNewestFirst(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Create(Meta{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(Meta{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0].ID != second.ID() || metas[1].ID != first.ID() {
		t.Fatalf("List = %+v", metas)
	}
}

func TestStoreDelete(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(session.ID()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Open(session.ID()); err != ErrNotFound {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
	if err := store.Delete("../../etc"); err != ErrNotFound {
		t.Fatalf("Delete with a path = %v, want ErrNotFound", err)
	}
	if err := store.Delete("missing"); err != ErrNotFound {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
}

func TestStoreOpenRejectsTraversal(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Open("../secret"); err != ErrNotFound {
		t.Fatalf("Open traversal = %v, want ErrNotFound", err)
	}
}

func TestSessionMetaPersistsAfterRestart(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "before", Provider: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.UpdateMeta(func(meta *Meta) {
		meta.Title = "after"
		meta.ClaudeSessionID = "abc-123"
		meta.Usage.InputTokens = 7
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Open(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta := again.Meta()
	if meta.Title != "after" || meta.ClaudeSessionID != "abc-123" || meta.Usage.InputTokens != 7 {
		t.Fatalf("meta after restart = %+v", meta)
	}
}

func TestTitleFrom(t *testing.T) {
	cases := map[string]string{
		"":                      "New conversation",
		"   ":                   "New conversation",
		"hello\nworld":          "hello world",
		strings.Repeat("a", 50): strings.Repeat("a", TitleLimit) + "…",
		"short title":           "short title",
	}
	for input, want := range cases {
		if got := TitleFrom(input); got != want {
			t.Errorf("TitleFrom(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSessionFilesArePrivate(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(EventUser, map[string]string{"text": "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Root(), session.ID(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("events.jsonl mode = %v, want 0600", mode)
	}
}

func TestStoreCountsTurnsOfOlderConversations(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Title: "old"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{EventUser, EventToken, EventResult, EventUser} {
		if _, err := session.Append(kind, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh store sees the conversation as written before Turns existed.
	reopened, err := NewStore(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	metas, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Turns != 2 {
		t.Fatalf("List = %+v", metas)
	}
	persisted, err := readMeta(filepath.Join(store.Root(), session.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Turns != 2 {
		t.Fatalf("persisted Turns = %d, want 2", persisted.Turns)
	}
}

func TestPinAndArchiveKeepTheOrder(t *testing.T) {
	base := testServer(t, Config{Token: "secret"}).URL
	var first, second SessionView
	_, body := doJSON(t, "POST", base+"/api/sessions", "secret", `{"title":"first"}`)
	_ = json.Unmarshal(body, &first)
	time.Sleep(10 * time.Millisecond)
	_, body = doJSON(t, "POST", base+"/api/sessions", "secret", `{"title":"second"}`)
	_ = json.Unmarshal(body, &second)

	status, body := doJSON(t, "PATCH", base+"/api/sessions/"+first.ID, "secret", `{"pinned":true,"archived":true}`)
	var patched SessionView
	if status != http.StatusOK || json.Unmarshal(body, &patched) != nil || !patched.Pinned || !patched.Archived {
		t.Fatalf("patch = %d %s", status, body)
	}
	if !patched.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("pinning moved the conversation: %v -> %v", first.UpdatedAt, patched.UpdatedAt)
	}
	var list []SessionView
	_, body = doJSON(t, "GET", base+"/api/sessions", "secret", "")
	_ = json.Unmarshal(body, &list)
	if len(list) != 2 || list[0].ID != second.ID || !list[1].Pinned {
		t.Fatalf("list = %s", body)
	}
	if status, body = doJSON(t, "PATCH", base+"/api/sessions/"+first.ID, "secret", `{"archived":false}`); status != http.StatusOK || strings.Contains(string(body), `"archived"`) {
		t.Fatalf("unarchive = %d %s", status, body)
	}
}
