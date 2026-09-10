package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// newJCCMirror is the mirror with its pair typed jcc, which is the type the hard
// exclusions and the lock gate apply to.
func newJCCMirror(t *testing.T) *mirror {
	t.Helper()

	m := newMirror(t)
	m.updatePair(t, url.Values{"type": {store.PairJCC}})
	return m
}

func (m *mirror) here(t *testing.T, rel string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(m.dst, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return body
}

// TestDashboardSyncsTheDatabase is the whole of DESIGN.md §3 from the operator's
// side: the covers transfer, the per-user
// databases do not, and the shared one goes through the gate at the end of the
// run rather than through the queue.
func TestDashboardSyncsTheDatabase(t *testing.T) {
	m := newJCCMirror(t)

	first := m.write(t, "ClipCornDB.db", 4096)
	cover := m.write(t, "cover/a.jpg", 512)
	m.write(t, "ClipCornUserData.db", 256)
	m.write(t, "ClipCornHistory.db", 256)

	mine := []byte("this side's own ratings")
	if err := os.WriteFile(filepath.Join(m.dst, "ClipCornUserData.db"), mine, 0o644); err != nil {
		t.Fatalf("write our own user data: %v", err)
	}

	m.run(t, RunScan)
	sync := m.run(t, RunSync)
	if sync.Error != "" {
		t.Fatalf("sync: %s", sync.Error)
	}
	if sync.Database == nil || !sync.Database.Replaced {
		t.Fatalf("the run did not replace the database: %+v", sync.Database)
	}
	if !strings.Contains(sync.Summary, "ClipCornDB.db replaced") {
		t.Errorf("summary = %q, want it to mention the database", sync.Summary)
	}

	if got := m.here(t, "ClipCornDB.db"); !bytes.Equal(got, first) {
		t.Errorf("the database here is %d bytes, want the publisher's %d", len(got), len(first))
	}
	if got := m.here(t, "cover/a.jpg"); !bytes.Equal(got, cover) {
		t.Error("the cover did not transfer, and it is an ordinary file of the pair")
	}
	if got := m.here(t, "ClipCornUserData.db"); !bytes.Equal(got, mine) {
		t.Error("ClipCornUserData.db was overwritten with the publisher's copy")
	}
	if _, err := os.Stat(filepath.Join(m.dst, "ClipCornHistory.db")); err == nil {
		t.Error("ClipCornHistory.db was transferred, and it is per-user too")
	}

	// A second run has nothing to do with it, which is what says the mtime made it
	// across: without that the database would transfer on every cycle forever.
	m.run(t, RunScan)
	again := m.run(t, RunSync)
	if again.Database == nil || again.Database.Replaced {
		t.Errorf("the second run copied the database again: %+v", again.Database)
	}
}

// The gate's answer to a lock is "not this cycle", so something has to bring the
// pair back around: its queue is empty and its manifest has not changed.
func TestDashboardDefersTheDatabaseWhileLocked(t *testing.T) {
	m := newJCCMirror(t)
	m.write(t, "ClipCornDB.db", 2048)
	m.write(t, "ClipCornDB.db.~lock", 4)
	m.run(t, RunScan)

	run := m.run(t, RunDatabase)
	if run.Error != "" {
		t.Fatalf("db run: %s", run.Error)
	}
	if run.Database == nil || run.Database.Deferred == "" {
		t.Fatalf("a held lock did not defer the copy: %+v", run.Database)
	}
	if _, err := os.Stat(filepath.Join(m.dst, "ClipCornDB.db")); err == nil {
		t.Error("the database was copied although the publisher had it open")
	}

	if !m.app.databaseDue(m.pair, time.Now().Add(databaseRetry+time.Minute)) {
		t.Error("the scheduler will not come back to a database it deferred")
	}

	// And the override is a person's to give.
	form := url.Values{
		"kind": {RunDatabase}, "pair": {strconv.FormatInt(m.pair.ID, 10)}, "force": {"true"},
	}
	if rec := postForm(t, m.h, "/api/runs", form); rec.Code != http.StatusAccepted {
		t.Fatalf("forced db run: %d %s", rec.Code, rec.Body)
	}
	waitForRun(t, m)

	if _, err := os.Stat(filepath.Join(m.dst, "ClipCornDB.db")); err != nil {
		t.Errorf("the override did not copy the database: %v", err)
	}
	if m.app.databaseDue(m.pair, time.Now().Add(time.Hour)) {
		t.Error("the retry clock is still running after the database was copied")
	}
}

// TestDashboardRollsTheDatabaseBack is the recovery of S5 reachable without a
// shell on the NAS, which is the only place it would otherwise live.
func TestDashboardRollsTheDatabaseBack(t *testing.T) {
	m := newJCCMirror(t)

	first := m.write(t, "ClipCornDB.db", 1024)
	m.run(t, RunScan)
	m.run(t, RunSync)

	second := m.write(t, "ClipCornDB.db", 2048)
	m.run(t, RunScan)
	m.run(t, RunSync)
	if got := m.here(t, "ClipCornDB.db"); !bytes.Equal(got, second) {
		t.Fatalf("the second database did not land")
	}

	views, err := m.app.PairViews(context.Background())
	if err != nil {
		t.Fatalf("pair views: %v", err)
	}
	if len(views[0].Backups) != 1 {
		t.Fatalf("%d copies kept, want the one the replacement displaced", len(views[0].Backups))
	}

	// The copies are on the pairs view, because a rollback nobody can find is
	// not a recovery story.
	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/pairs", nil))
	var shown []PairView
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil {
		t.Fatalf("decode /api/pairs: %v", err)
	}
	if len(shown) != 1 || len(shown[0].Backups) != 1 {
		t.Fatalf("the pairs view does not carry the kept copy: %s", rec.Body)
	}

	form := url.Values{
		"id":     {strconv.FormatInt(m.pair.ID, 10)},
		"backup": {strconv.FormatInt(views[0].Backups[0].ID, 10)},
	}
	if rec := postForm(t, m.h, "/api/pairs/database/rollback", form); rec.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body)
	}

	if got := m.here(t, "ClipCornDB.db"); !bytes.Equal(got, first) {
		t.Errorf("the database was not put back: %d bytes, want %d", len(got), len(first))
	}
	// What the rollback displaced is kept too, so it is undoable.
	if views, err = m.app.PairViews(context.Background()); err != nil {
		t.Fatalf("pair views: %v", err)
	} else if len(views[0].Backups) != 2 {
		t.Errorf("%d copies kept after the rollback, want 2", len(views[0].Backups))
	}
}

// TestDatabaseRollbackIsOpen: the recovery of S5 needs no login either. A pair
// that is not there is an ordinary mistake, and is answered as one.
func TestDatabaseRollbackIsOpen(t *testing.T) {
	m := newJCCMirror(t)

	rec := postForm(t, m.h, "/api/pairs/database/rollback", url.Values{"id": {"999999"}})
	if rec.Code < 400 || rec.Code >= 500 || rec.Code == http.StatusUnauthorized {
		t.Fatalf("a rollback of a pair that does not exist = %d, want a readable 4xx: %s", rec.Code, rec.Body)
	}

	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Error == "" {
		t.Errorf("the refusal carries nothing to read: %s", rec.Body)
	}
}

// waitForRun blocks until the runner is idle again, the way m.run does for the
// runs it starts itself.
func waitForRun(t *testing.T, m *mirror) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if state := m.app.Runs(context.Background()); state.Current == nil && len(state.History) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the run did not finish")
}
