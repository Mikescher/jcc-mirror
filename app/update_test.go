package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/update"
)

// publishBinary puts a release on the publisher's share. It is a copy of the
// test binary, so it is a real ELF and answers `--version` the way the smoke
// test needs - which is what lets this exercise the updater rather than a stub.
func publishBinary(t *testing.T, m *mirror, rel string) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read the test binary: %v", err)
	}

	full := filepath.Join(m.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o755); err != nil {
		t.Fatal(err)
	}
}

func updateStatus(t *testing.T, m *mirror) UpdateStatus {
	t.Helper()
	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/update = %d: %s", rec.Code, rec.Body)
	}
	var st UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return st
}

// TestUpdateIsOffUntilItIsConfigured: an install that was never told where a
// binary lives must never reach for one, and must not report that as a fault.
func TestUpdateIsOffUntilItIsConfigured(t *testing.T) {
	m := newMirror(t)

	st := updateStatus(t, m)
	if st.Configured || st.Newer || st.Error != "" {
		t.Fatalf("a fresh install reports %+v", st)
	}
	if st.Version != "test" {
		t.Errorf("version = %q", st.Version)
	}

	if rec := postForm(t, m.h, "/api/update/check", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a check with no URL = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// TestUpdateInstallsAndAsksForTheRestart is DESIGN.md §5 end to end: HEAD,
// download, verify, smoke-test, rotate, and the re-exec request the daemon's own
// main loop waits on.
func TestUpdateInstallsAndAsksForTheRestart(t *testing.T) {
	m := newMirror(t)
	publishBinary(t, m, "dist/jcc-mirror-amd64")

	if rec := postForm(t, m.h, "/api/config",
		url.Values{store.KeyUpdateURL: {"dist/jcc-mirror-amd64"}}); rec.Code != http.StatusOK {
		t.Fatalf("configure the update URL: %d %s", rec.Code, rec.Body)
	}

	if rec := postForm(t, m.h, "/api/update/check", nil); rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	st := updateStatus(t, m)
	if !st.Configured || !st.Newer || st.Available == nil {
		t.Fatalf("the check found nothing: %+v", st)
	}
	if st.Blocked != "" {
		t.Errorf("blocked = %q", st.Blocked)
	}

	if rec := postForm(t, m.h, "/api/update/apply", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}

	// The daemon does not exec itself: it asks, and cmdServe does it once every
	// listener is shut. What is asserted here is the ask.
	select {
	case binary := <-m.app.Restart():
		if binary != m.app.upd.manager.Binary() {
			t.Errorf("restart into %q, want %q", binary, m.app.upd.manager.Binary())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the update did not ask to be restarted into")
	}

	state, ok, err := m.app.upd.manager.LoadState()
	if err != nil || !ok {
		t.Fatalf("state: %v, %v", ok, err)
	}
	if state.State != update.StateApplied || state.ToVersion != "v-next" || state.FromVersion != "test" {
		t.Fatalf("state = %+v", state)
	}
	if !m.app.upd.manager.Installed() {
		t.Error("nothing was installed")
	}

	// A second apply is refused, and as a conflict rather than as a fault: the
	// baseline is now what was installed, not the build stamp, so the same binary
	// does not read as newer than itself.
	rec := postForm(t, m.h, "/api/update/apply", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("a second apply = %d, want 409: %s", rec.Code, rec.Body)
	}
}

// TestUpdateRefusesWhatIsNotABinary: the checks are aimed at corruption, and an
// error page saved where the binary should be is the shape it usually takes.
func TestUpdateRefusesWhatIsNotABinary(t *testing.T) {
	m := newMirror(t)
	m.write(t, "dist/jcc-mirror-amd64", 4096) // random bytes, no ELF magic

	if rec := postForm(t, m.h, "/api/config",
		url.Values{store.KeyUpdateURL: {"dist/jcc-mirror-amd64"}}); rec.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body)
	}

	rec := postForm(t, m.h, "/api/update/apply", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("apply = %d, want 502: %s", rec.Code, rec.Body)
	}
	if m.app.upd.manager.Installed() {
		t.Fatal("something that is not a binary was installed")
	}

	events, err := m.app.store.Events(context.Background(),
		store.EventFilter{Kinds: []string{store.KindUpdateFailed}, Limit: 5})
	if err != nil || len(events) == 0 {
		t.Fatalf("the failure was not recorded: %v (%d events)", err, len(events))
	}
}

// TestRolledBackUpdateIsNotFetchedAgain closes the one loop a self-updater must
// not be able to get into: install, crash, roll back, install the same thing
// again on the next check.
func TestRolledBackUpdateIsNotFetchedAgain(t *testing.T) {
	m := newMirror(t)
	publishBinary(t, m, "dist/jcc-mirror-amd64")
	if rec := postForm(t, m.h, "/api/config",
		url.Values{store.KeyUpdateURL: {"dist/jcc-mirror-amd64"}}); rec.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body)
	}

	if _, err := m.app.ApplyUpdate(context.Background(), false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	<-m.app.Restart()

	// What the supervisor would have done, had the new binary refused to start.
	if _, err := m.app.upd.manager.Rollback("it exited with status 1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, newer, err := m.app.CheckUpdate(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	} else if newer {
		t.Fatal("the binary that was just rolled back is offered again")
	}
	if st := updateStatus(t, m); st.Blocked == "" {
		t.Error("the panel does not say why it is not offered")
	}

	// A person can still insist, which is the difference between a guard and a
	// refusal.
	if _, err := m.app.ApplyUpdate(context.Background(), true); err != nil {
		t.Fatalf("forced apply: %v", err)
	}
}

// TestUpdateActionsAreOpen: the update buttons need no login, and on an install
// that was never told where a binary lives they say so rather than refusing the
// caller.
func TestUpdateActionsAreOpen(t *testing.T) {
	_, h := newApp(t)
	for _, path := range []string{"/api/update/check", "/api/update/apply", "/api/update/rollback"} {
		rec := postForm(t, h, path, nil)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("POST %s = 401: %s", path, rec.Body)
			continue
		}

		var answer struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Error == "" {
			t.Errorf("POST %s = %d with nothing to read: %s", path, rec.Code, rec.Body)
		}
	}
	// Reading the panel is open, like every other read view.
	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/update", nil)); rec.Code != http.StatusOK {
		t.Errorf("GET /api/update = %d", rec.Code)
	}
}

func TestRollbackWithNothingInstalledIsRefused(t *testing.T) {
	_, h := newApp(t)
	if rec := postForm(t, h, "/api/update/rollback", nil); rec.Code != http.StatusConflict {
		t.Errorf("rollback = %d, want 409: %s", rec.Code, rec.Body)
	}
}
