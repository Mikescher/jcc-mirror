package update

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/remote/localfs"
)

// The test binary doubles as the binary being installed. It is a real ELF, it
// can be copied into the binary directory and executed from there, and the two
// argv branches below make it answer `--version` like the real one and exit with
// a scripted status like a daemon that will not start. Building a fixture per
// test would be a `go build` for the same thing.
const (
	envVersion = "JCCMIRROR_TEST_VERSION"
	envExits   = "JCCMIRROR_TEST_EXITS"
	envCounter = "JCCMIRROR_TEST_COUNTER"
)

func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "--version":
		line := os.Getenv(envVersion)
		if line == "" {
			line = "jcc-mirror v9 (built 2030-01-01T00:00:00Z)"
		}
		fmt.Println(line)
		os.Exit(0)
	case len(os.Args) > 1 && os.Args[1] == "child":
		os.Exit(scriptedExit())
	}
	os.Exit(m.Run())
}

// scriptedExit consumes the next status from JCCMIRROR_TEST_EXITS, counting the
// runs in a file because each one is a fresh process.
func scriptedExit() int {
	codes := strings.Split(os.Getenv(envExits), ",")
	counter := os.Getenv(envCounter)

	n := 0
	if raw, err := os.ReadFile(counter); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	_ = os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o644)

	if n >= len(codes) {
		return 0
	}
	code, _ := strconv.Atoi(strings.TrimSpace(codes[n]))
	return code
}

func selfBytes(t *testing.T) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read the test binary: %v", err)
	}
	return body
}

// serve answers HEAD and GET for one body with one modification time, which is
// the whole of what the updater asks a share for.
func serve(t *testing.T, body []byte, mod time.Time) Source {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
		http.ServeContent(w, r, "jcc-mirror", mod, strings.NewReader(string(body)))
	}))
	t.Cleanup(srv.Close)
	return Source{URL: srv.URL + "/dist/jcc-mirror", HTTP: srv.Client()}
}

func TestHeadReportsWhatTheShareHolds(t *testing.T) {
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	src := serve(t, []byte("\x7fELF and then some"), mod)

	rel, err := src.Head(t.Context())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if !rel.ModTime.Equal(mod) {
		t.Errorf("mod time = %v, want %v", rel.ModTime, mod)
	}
	if rel.Size != int64(len("\x7fELF and then some")) {
		t.Errorf("size = %d, want %d", rel.Size, len("\x7fELF and then some"))
	}
}

// A share that reports no Last-Modified has to be an error: it is the only thing
// the comparison rests on, and an update that silently never happens is the
// failure least likely to be noticed.
func TestHeadRefusesAResponseWithoutALastModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Last-Modified"] = nil
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := Source{URL: srv.URL, HTTP: srv.Client()}
	if _, err := src.Head(t.Context()); err == nil || !strings.Contains(err.Error(), "Last-Modified") {
		t.Fatalf("err = %v, want one naming Last-Modified", err)
	}
}

func TestVerifyRejectsWhatIsNotABinary(t *testing.T) {
	dir := t.TempDir()

	page := filepath.Join(dir, "page")
	if err := os.WriteFile(page, []byte("<html>404 not found</html>"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(page, 0); err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Errorf("an HTML error page passed verification: %v", err)
	}

	cut := filepath.Join(dir, "cut")
	if err := os.WriteFile(cut, []byte("\x7fELF short"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(cut, 4096); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Errorf("a truncated download passed verification: %v", err)
	}
	if err := Verify(cut, int64(len("\x7fELF short"))); err != nil {
		t.Errorf("a whole file failed verification: %v", err)
	}
}

func TestSmokeTestReadsTheVersionBack(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "jcc-mirror.new")
	if err := os.WriteFile(candidate, selfBytes(t), 0o755); err != nil {
		t.Fatal(err)
	}

	version, build, err := SmokeTest(t.Context(), candidate)
	if err != nil {
		t.Fatalf("smoke test: %v", err)
	}
	if version != "v9" || build != "2030-01-01T00:00:00Z" {
		t.Errorf("version = %q, build = %q", version, build)
	}

	// The check that catches a well-formed ELF that is not ours - the case the
	// magic number cannot see.
	t.Setenv(envVersion, "This program cannot be run in DOS mode")
	if _, _, err := SmokeTest(t.Context(), candidate); err == nil {
		t.Error("a binary that is not a jcc-mirror passed the smoke test")
	}
}

func TestApplyInstallsAndRollbackUndoesIt(t *testing.T) {
	m := Manager{Dir: filepath.Join(t.TempDir(), "bin")}
	src := serve(t, selfBytes(t), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	rel, err := src.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// The first update replaces the binary in the image, so there is nothing to
	// keep as the previous one and the way back is an empty directory.
	res, err := m.Apply(t.Context(), src, rel, "v1")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !m.Installed() || res.Exec != m.Binary() {
		t.Fatalf("exec = %q, installed = %v", res.Exec, m.Installed())
	}
	if m.HasPrevious() {
		t.Error("a first update left a previous binary behind")
	}
	if res.State.State != StateApplied || res.State.ToVersion != "v9" || res.State.FromVersion != "v1" {
		t.Errorf("state = %+v", res.State)
	}
	if _, err := os.Stat(m.Incoming()); err == nil {
		t.Error("the download was left in place")
	}

	// The second one has something to keep.
	second := serve(t, selfBytes(t), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	rel2, err := second.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(t.Context(), second, rel2, "v9"); err != nil {
		t.Fatalf("apply again: %v", err)
	}
	if !m.HasPrevious() {
		t.Fatal("the replaced binary was not kept")
	}

	if _, err := m.Rollback("because"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !m.Installed() || m.HasPrevious() {
		t.Error("the rollback did not move the previous binary back into place")
	}

	// And a rollback with nothing left to restore falls back to the image's
	// binary, which is what an absent managed one means.
	back, err := m.Rollback("again")
	if err != nil {
		t.Fatalf("rollback again: %v", err)
	}
	if m.Installed() || back.Exec != "" {
		t.Errorf("exec = %q, installed = %v", back.Exec, m.Installed())
	}
	if st, _, _ := m.LoadState(); st.State != StateRolledBack {
		t.Errorf("state = %q, want %q", st.State, StateRolledBack)
	}
}

// The baseline is the reason a binary built at 10:00 and uploaded at 10:05 does
// not read as an update to itself forever.
func TestBaselineIsTheInstalledTimestamp(t *testing.T) {
	m := Manager{Dir: t.TempDir()}
	build := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if got := m.Baseline(build); !got.Equal(build) {
		t.Errorf("with no state, baseline = %v, want the build time", got)
	}

	uploaded := build.Add(5 * time.Minute)
	if err := m.SaveState(State{State: StateConfirmed, RemoteModTime: uploaded}); err != nil {
		t.Fatal(err)
	}
	if got := m.Baseline(build); !got.Equal(uploaded) {
		t.Errorf("baseline = %v, want the upload time %v", got, uploaded)
	}

	// After a rollback the running binary is the previous one again, so its own
	// build stamp is the only meaningful baseline.
	if err := m.SaveState(State{State: StateRolledBack, RemoteModTime: uploaded}); err != nil {
		t.Fatal(err)
	}
	if got := m.Baseline(build); !got.Equal(build) {
		t.Errorf("after a rollback, baseline = %v, want the build time", got)
	}
}

func TestBlockedNamesTheUpdateThatWasRolledBack(t *testing.T) {
	m := Manager{Dir: t.TempDir()}
	if _, ok := m.Blocked(); ok {
		t.Fatal("a fresh install reports a blocked update")
	}

	mod := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := m.SaveState(State{State: StateRolledBack, RemoteModTime: mod, RollbackReason: "it exited"}); err != nil {
		t.Fatal(err)
	}
	st, ok := m.Blocked()
	if !ok || st.RollbackReason != "it exited" {
		t.Fatalf("blocked = %v, %+v", ok, st)
	}

	if _, err := m.Confirm(st); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Blocked(); !ok {
		t.Error("Confirm cleared a rollback it should not have touched")
	}
}

func TestConfirmClosesTheRollbackWindow(t *testing.T) {
	m := Manager{Dir: t.TempDir()}
	applied := State{State: StateApplied, UpdatedAt: time.Now().UTC()}
	if err := m.SaveState(applied); err != nil {
		t.Fatal(err)
	}

	confirmed, err := m.Confirm(applied)
	if err != nil || !confirmed {
		t.Fatalf("confirmed = %v, err = %v", confirmed, err)
	}
	if st, _, _ := m.LoadState(); st.State != StateConfirmed {
		t.Errorf("state = %q", st.State)
	}
	if confirmed, _ := m.Confirm(applied); confirmed {
		t.Error("a second confirm reported a transition")
	}

	// A process that installs a second update just before its own minute is up
	// must not sign off on the one it has never run.
	second := State{State: StateApplied, UpdatedAt: applied.UpdatedAt.Add(time.Minute)}
	if err := m.SaveState(second); err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := m.Confirm(applied); confirmed {
		t.Error("an update confirmed one it did not run")
	}
}

func TestLocate(t *testing.T) {
	if _, _, err := Locate("  "); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
	if url, path, _ := Locate("http://elsewhere/bin"); url != "http://elsewhere/bin" || path != "" {
		t.Errorf("absolute = (%q, %q)", url, path)
	}
	// Not escaped and not joined to anything: it is a path on the share, which is
	// what the remote client is handed verbatim.
	if url, path, _ := Locate(" dist/jcc mirror-amd64 "); url != "" || path != "dist/jcc mirror-amd64" {
		t.Errorf("share-relative = (%q, %q)", url, path)
	}
}

// TestHeadAndDownloadOffTheShare is the form the deployment actually uses: the
// binary sits on the publisher's share, so the updater stats and reads it with
// the same client the mirror reads everything else with.
func TestHeadAndDownloadOffTheShare(t *testing.T) {
	dir := t.TempDir()
	body := []byte("\x7fELF and then some")
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "dist", "jcc-mirror-amd64")
	if err := os.WriteFile(bin, body, 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(bin, mod, mod); err != nil {
		t.Fatal(err)
	}

	share, err := localfs.New(dir)
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	src := Source{Path: "dist/jcc-mirror-amd64", Share: share}

	rel, err := src.Head(t.Context())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if !rel.ModTime.Equal(mod) {
		t.Errorf("mod time = %v, want %v", rel.ModTime, mod)
	}
	if rel.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", rel.Size, len(body))
	}
	if rel.URL != "dist/jcc-mirror-amd64" {
		t.Errorf("release names %q, want the path on the share", rel.URL)
	}

	dst := filepath.Join(t.TempDir(), "bin", "candidate")
	n, err := src.Download(t.Context(), dst)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("downloaded %d bytes, want %d", n, len(body))
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(body) {
		t.Fatalf("downloaded %q (%v), want the file on the share", got, err)
	}
}

// A share-relative binary with no client to read it with has to say so rather
// than fall back to fetching nothing: an update that silently never happens is
// the failure least likely to be noticed.
func TestShareRelativeBinaryNeedsARemote(t *testing.T) {
	src := Source{Path: "dist/jcc-mirror-amd64"}
	if _, err := src.Head(t.Context()); !errors.Is(err, ErrNoRemote) {
		t.Errorf("head err = %v, want ErrNoRemote", err)
	}
	if _, err := src.Download(t.Context(), filepath.Join(t.TempDir(), "bin")); !errors.Is(err, ErrNoRemote) {
		t.Errorf("download err = %v, want ErrNoRemote", err)
	}
}

func TestParseBuildStamp(t *testing.T) {
	if got := ParseBuildStamp("2026-09-01T10:00:00Z"); !got.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	// "unknown" is what a build with no Makefile behind it carries. It must read
	// as "cannot tell" rather than as the epoch, or everything looks newer.
	for _, in := range []string{"", "unknown", "not a date"} {
		if got := ParseBuildStamp(in); !got.IsZero() {
			t.Errorf("ParseBuildStamp(%q) = %v, want the zero time", in, got)
		}
	}
}
