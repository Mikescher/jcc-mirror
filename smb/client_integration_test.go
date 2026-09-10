package smb

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/remote/localfs"
)

// The environment that turns the integration test on. It needs a real SMB server
// because there is no in-process one, and it needs JCC_SMB_LOCAL_DIR - the
// directory that server exports - because the point of the test is that the two
// remotes answer alike about the same tree.
//
//	JCC_SMB_HOST=10.13.13.2 JCC_SMB_SHARE=media JCC_SMB_USER=ro \
//	JCC_SMB_PASS=... JCC_SMB_LOCAL_DIR=/volume1/media go test ./smb/
const (
	envHost   = "JCC_SMB_HOST"
	envShare  = "JCC_SMB_SHARE"
	envPath   = "JCC_SMB_PATH"
	envUser   = "JCC_SMB_USER"
	envPass   = "JCC_SMB_PASS"
	envDomain = "JCC_SMB_DOMAIN"
	envLocal  = "JCC_SMB_LOCAL_DIR"
)

// integrationClient builds the client from the environment and returns the local
// path of the same tree, so the two can be compared.
func integrationClient(t *testing.T) (*Client, string) {
	t.Helper()

	host, share, local := os.Getenv(envHost), os.Getenv(envShare), os.Getenv(envLocal)
	if host == "" || share == "" || local == "" {
		t.Skipf("set %s, %s and %s to run this against a real SMB server", envHost, envShare, envLocal)
	}

	c, err := New(Config{
		Host:     host,
		Share:    share,
		Path:     os.Getenv(envPath),
		User:     os.Getenv(envUser),
		Password: os.Getenv(envPass),
		Domain:   os.Getenv(envDomain),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// The base path is part of the client, so the local side has to be rooted at
	// the same place or every comparison below is between two different trees.
	if p := os.Getenv(envPath); p != "" {
		local = filepath.Join(local, filepath.FromSlash(p))
	}
	return c, local
}

// TestAgreesWithTheFake is the reason localfs exists: the engine is developed
// against it and deployed against DSM, so the two have to answer alike about the
// same tree. The fixture is written on the local side of the share, which is why
// the test needs the server's own directory as well as its address.
func TestAgreesWithTheFake(t *testing.T) {
	client, local := integrationClient(t)
	ctx := context.Background()

	root := filepath.Join(local, "jcc-mirror-integration")
	if err := os.MkdirAll(filepath.Join(root, "Filme A-Z"), 0o755); err != nil {
		t.Fatalf("mkdir on the share's own filesystem: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	payload := make([]byte, 40<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	// A decomposed name: what the walk has to survive is a tree that came past
	// macOS, and both sides must report the composed form (DESIGN.md §2.3).
	const name = "Grüße.mkv"
	if err := os.WriteFile(filepath.Join(root, "Filme A-Z", name), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake, err := localfs.New(local)
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}

	const composed = "jcc-mirror-integration/Filme A-Z/Grüße.mkv"

	for _, dir := range []string{"jcc-mirror-integration", "jcc-mirror-integration/Filme A-Z"} {
		a, err := fake.List(ctx, dir)
		if err != nil {
			t.Fatalf("fake list %q: %v", dir, err)
		}
		b, err := client.List(ctx, dir)
		if err != nil {
			t.Fatalf("smb list %q: %v", dir, err)
		}
		if !sameEntries(a, b) {
			t.Errorf("listing of %q differs:\n fake %+v\n smb  %+v", dir, a, b)
		}
	}

	// SMB carries a Windows FILETIME, so its mtime is finer than the fake's
	// whole seconds. Truncating is the comparison the differ makes anyway.
	st, err := client.Stat(ctx, composed)
	if err != nil {
		t.Fatalf("smb stat: %v", err)
	}
	local2, err := fake.Stat(ctx, composed)
	if err != nil {
		t.Fatalf("fake stat: %v", err)
	}
	if st.Size != local2.Size || st.Path != local2.Path || st.Name != local2.Name {
		t.Errorf("stat differs:\n fake %+v\n smb  %+v", local2, st)
	}
	if got := st.MTime.Truncate(time.Second); !got.Equal(local2.MTime) {
		t.Errorf("mtime = %v, want %v to the second", st.MTime, local2.MTime)
	}

	// The bounded form is the one the transfer engine issues, several at a time
	// into one file: a chunk that came back longer or shorter than asked for
	// would be written straight over its neighbour.
	for _, c := range []struct{ offset, length int64 }{
		{0, 1024},
		{4096, 8192},
		{int64(len(payload)) - 10, 10},
		{0, 0},
		{1, 0},
	} {
		got := readRange(t, client, composed, c.offset, c.length)
		want := payload[c.offset:]
		if c.length > 0 {
			want = payload[c.offset : c.offset+c.length]
		}
		if !bytes.Equal(got, want) {
			t.Errorf("range %d+%d returned %d bytes, want %d", c.offset, c.length, len(got), len(want))
		}
	}

	// A read past the end is an error rather than an empty body: an empty body at
	// a resume offset looks exactly like a finished file.
	if _, _, err := client.OpenRange(ctx, composed, int64(len(payload))+1, 0); err == nil {
		t.Error("a read past the end succeeded")
	}
}

// TestExistsTellsAbsenceFromFailure is the lock gate of DESIGN.md §3: a path
// that is not there is false and no error, and nothing else may be.
func TestExistsTellsAbsenceFromFailure(t *testing.T) {
	client, local := integrationClient(t)
	ctx := context.Background()

	lock := filepath.Join(local, "jcc-mirror-integration.~lock")
	t.Cleanup(func() { os.Remove(lock) })

	switch found, err := client.Exists(ctx, "jcc-mirror-integration.~lock"); {
	case err != nil:
		t.Fatalf("Exists on a missing path errored: %v", err)
	case found:
		t.Fatal("Exists found a file that is not there")
	}

	if err := os.WriteFile(lock, []byte("4711"), 0o644); err != nil {
		t.Fatal(err)
	}
	switch found, err := client.Exists(ctx, "jcc-mirror-integration.~lock"); {
	case err != nil:
		t.Fatalf("Exists on a present path errored: %v", err)
	case !found:
		t.Fatal("Exists missed a file that is there")
	}
}

// TestSurvivesADroppedSession is the property a sync running for days rests on:
// the session is closed underneath the client, and the next operation brings a
// new one up rather than failing the run.
func TestSurvivesADroppedSession(t *testing.T) {
	client, _ := integrationClient(t)
	ctx := context.Background()

	if _, err := client.List(ctx, ""); err != nil {
		t.Fatalf("first list: %v", err)
	}

	// What a tunnel blip does, from the client's side.
	client.mu.Lock()
	conn := client.conn
	client.mu.Unlock()
	if conn == nil {
		t.Fatal("no session was kept after the first list")
	}
	conn.Close()

	if _, err := client.List(ctx, ""); err != nil {
		t.Errorf("the list after a dropped session failed rather than re-dialing: %v", err)
	}
}

func sameEntries(a, b []remote.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		// The timestamps are compared to the second: the fake truncates and SMB
		// does not, which is the one difference the two are allowed to have.
		x.MTime, y.MTime = x.MTime.Truncate(time.Second), y.MTime.Truncate(time.Second)
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
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
