package localfs

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/remote"
)

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

// fixture is a tree shaped like the real one: an umlaut, a space, and the
// jClipCorn database directory.
func fixture(t *testing.T) (dir string, payload []byte) {
	t.Helper()
	dir = t.TempDir()
	payload = make([]byte, 64*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	writeFile(t, dir, "Filme A-Z/Grüße.mkv", payload)
	writeFile(t, dir, "ClipCornDB/ClipCornDB.db", []byte("not really a database"))
	return dir, payload
}

func TestListStatOpen(t *testing.T) {
	dir, payload := fixture(t)
	fs, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	root, err := fs.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(root) != 2 {
		t.Fatalf("root has %d entries, want 2: %+v", len(root), root)
	}
	for _, e := range root {
		if !e.IsDir {
			t.Errorf("%q should be a directory", e.Path)
		}
		if e.Size != 0 {
			t.Errorf("directory %q reports size %d", e.Path, e.Size)
		}
	}

	e, err := fs.Stat(ctx, "Filme A-Z/Grüße.mkv")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", e.Size, len(payload))
	}
	if e.Name != "Grüße.mkv" {
		t.Errorf("name = %q", e.Name)
	}
	// Whole seconds, because that is all getlastmodified can carry - a fake with
	// finer timestamps would hide the very bug the differ has to survive.
	if e.MTime != e.MTime.Truncate(time.Second) {
		t.Errorf("mtime %v is finer than a second", e.MTime)
	}

	rc, err := fs.Open(ctx, "Filme A-Z/Grüße.mkv", 1000)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, payload[1000:]) {
		t.Errorf("read %d bytes from offset 1000, want %d matching", len(got), len(payload)-1000)
	}
}

func TestOpenPastEndFails(t *testing.T) {
	dir, payload := fixture(t)
	fs, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The server answers 416 rather than an empty body, and a resume that silently
	// read zero bytes would look like a finished file.
	if _, err := fs.Open(context.Background(), "Filme A-Z/Grüße.mkv", int64(len(payload))+1); err == nil {
		t.Fatal("Open past the end succeeded, want an error")
	}
}

func TestPathsCannotEscapeTheRoot(t *testing.T) {
	dir, _ := fixture(t)
	writeFile(t, filepath.Dir(dir), "outside.txt", []byte("not yours"))

	fs, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, p := range []string{"../outside.txt", "ClipCornDB/../../outside.txt"} {
		if _, err := fs.Stat(context.Background(), p); err == nil {
			t.Errorf("Stat(%q) succeeded, want an error", p)
		}
	}
}

func listSorted(ctx context.Context, r remote.Remote, dir string) ([]remote.Entry, error) {
	entries, err := r.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	sortEntries(entries)
	return entries, nil
}

func sortEntries(entries []remote.Entry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Path < entries[j-1].Path; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func readRange(t *testing.T, r remote.RangeReader, path string, offset, length int64) []byte {
	t.Helper()
	rc, _, err := r.OpenRange(context.Background(), path, offset, length)
	if err != nil {
		t.Fatalf("open %q at %d+%d: %v", path, offset, length, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

func readAll(t *testing.T, r remote.Remote, path string, offset int64) []byte {
	t.Helper()
	rc, err := r.Open(context.Background(), path, offset)
	if err != nil {
		t.Fatalf("open %q at %d: %v", path, offset, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}
