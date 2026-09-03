package webdav

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	xwebdav "golang.org/x/net/webdav"
)

// newTestServer serves dir over WebDAV under /share, which is the local fake the
// design asks for: the whole client can be exercised with no network and no NAS.
func newTestServer(t *testing.T, dir string) *Client {
	t.Helper()

	h := &xwebdav.Handler{
		Prefix:     "/share",
		FileSystem: xwebdav.Dir(dir),
		LockSystem: xwebdav.NewMemLS(),
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	client, err := New(Config{BaseURL: srv.URL + "/share"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestClientListStatOpen(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 64*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	writeFile(t, dir, "Filme A-Z/Grüße.mkv", payload)
	writeFile(t, dir, "ClipCornDB/ClipCornDB.db", []byte("not really a database"))

	client := newTestServer(t, dir)
	ctx := context.Background()

	root, err := client.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(root) != 2 {
		t.Fatalf("List(root) returned %d entries, want 2: %+v", len(root), root)
	}
	for _, e := range root {
		if !e.IsDir {
			t.Errorf("%q should be a directory", e.Path)
		}
	}

	files, err := client.List(ctx, "Filme A-Z")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 1 || files[0].Path != "Filme A-Z/Grüße.mkv" {
		t.Fatalf("List(Filme A-Z) = %+v, want the one file with its full relative path", files)
	}
	if files[0].Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", files[0].Size, len(payload))
	}

	entry, err := client.Stat(ctx, "Filme A-Z/Grüße.mkv")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if entry.Size != int64(len(payload)) || entry.IsDir {
		t.Errorf("Stat = %+v, want a file of %d bytes", entry, len(payload))
	}

	body, err := client.Open(ctx, "Filme A-Z/Grüße.mkv", 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("full read returned %d bytes, want %d", len(got), len(payload))
	}
}

// TestClientOpenRange is the property the transfer engine is built on: a read that
// starts in the middle of a file returns exactly the bytes at that offset.
func TestClientOpenRange(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = byte(i)
	}
	writeFile(t, dir, "big.bin", payload)

	client := newTestServer(t, dir)
	ctx := context.Background()

	body, total, err := client.OpenRange(ctx, "big.bin", 4096, 1024)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if total != int64(len(payload)) {
		t.Errorf("total = %d, want %d", total, len(payload))
	}
	if len(got) != 1024 {
		t.Fatalf("read %d bytes, want 1024", len(got))
	}
	if string(got) != string(payload[4096:5120]) {
		t.Errorf("ranged read returned the wrong bytes")
	}
}

// TestClientOpenRangeIgnored covers a server that answers 200 to a ranged request.
// Treating that as a short read would write the start of the file at the resume
// offset and corrupt it without any error at all, so it has to fail loudly.
func TestClientOpenRangeIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("the whole file, from the start"))
	}))
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body, _, err := client.OpenRange(context.Background(), "anything", 100, 0)
	if err == nil {
		body.Close()
		t.Fatal("OpenRange succeeded against a server that ignored Range, want an error")
	}
}

// TestClientOpenRangeIgnoredAtZero is the harmless half of the same situation: a
// range starting at zero that the server ignores still delivers the right bytes,
// so it is capped rather than refused. Refusing it would break a plain download
// against any server that answers 200 to a whole-file range.
func TestClientOpenRangeIgnoredAtZero(t *testing.T) {
	const full = "the whole file, from the start"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(full))
	}))
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body, _, err := client.OpenRange(context.Background(), "anything", 0, 10)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != full[:10] {
		t.Errorf("got %q, want %q", got, full[:10])
	}
}

// TestClientRedirectOnPropfind covers the trap of Go rewriting a redirected
// PROPFIND into a GET, which would arrive as an incomprehensible decode error.
func TestClientRedirectOnPropfind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.List(context.Background(), ""); err == nil {
		t.Fatal("List followed a redirect, want an error naming it")
	}
}

func TestClientExists(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ClipCornDB/ClipCornDB.db.~lock", []byte("4711"))

	client := newTestServer(t, dir)
	ctx := context.Background()

	ok, err := client.Exists(ctx, "ClipCornDB/ClipCornDB.db.~lock")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("the lock file should exist")
	}

	ok, err = client.Exists(ctx, "ClipCornDB/ClipCornDB.db")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Error("the database should not exist")
	}
}

func TestClientProbeDepthInfinity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a/b/c.txt", []byte("x"))

	client := newTestServer(t, dir)

	honoured, reason, err := client.ProbeDepthInfinity(context.Background(), "")
	if err != nil {
		t.Fatalf("ProbeDepthInfinity: %v", err)
	}
	if reason == "" {
		t.Error("the probe should report what the server said either way")
	}
	t.Logf("x/net/webdav honours depth infinity: %v (%s)", honoured, reason)
}
