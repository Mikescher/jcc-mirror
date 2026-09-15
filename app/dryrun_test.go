package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func (m *mirror) dryRunPage(t *testing.T, query string) DryRunPage {
	t.Helper()
	path := "/api/runs/dryrun?pair=" + strconv.FormatInt(m.pair.ID, 10) + query
	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var page DryRunPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return page
}

// TestDryRunWalksAndListsEverything needs no scan before it: the walk is part of
// the run, and the list comes back complete however long it is.
func TestDryRunWalksAndListsEverything(t *testing.T) {
	m := newMirror(t)
	m.write(t, "b/two.mkv", 2048)
	m.write(t, "a/one.mkv", 1024)
	m.write(t, "c/three.mkv", 4096)

	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/runs/dryrun?pair="+strconv.FormatInt(m.pair.ID, 10), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("before any dry run: %d, want 404", rec.Code)
	}

	run := m.run(t, RunDryRun)
	if run.Error != "" || run.Plan == nil {
		t.Fatalf("dry run: %s", run.Error)
	}
	if run.Plan.Add != 3 {
		t.Errorf("plan wants %d adds, want 3", run.Plan.Add)
	}
	if len(run.Plan.Entries) != 0 {
		t.Errorf("the run carries %d entries; the list belongs to the endpoint, not the stream", len(run.Plan.Entries))
	}
	if run.Phase != "" {
		t.Errorf("a finished run still says it is %q", run.Phase)
	}

	if left, err := os.ReadDir(m.dst); err != nil || len(left) != 0 {
		t.Errorf("the destination was touched: %v, %d entries", err, len(left))
	}
	if q, err := m.app.store.Queue(t.Context(), m.pair.ID); err != nil || len(q.Counts) != 0 {
		t.Errorf("the queue was written: %v, %v", err, q.Counts)
	}

	all := m.dryRunPage(t, "")
	if all.Total != 3 || len(all.Entries) != 3 {
		t.Fatalf("list: total %d, %d entries, want 3", all.Total, len(all.Entries))
	}
	if all.Entries[0].Path != "a/one.mkv" || all.Entries[2].Path != "c/three.mkv" {
		t.Errorf("list is not sorted by path: %+v", all.Entries)
	}

	page := m.dryRunPage(t, "&offset=1&limit=1")
	if page.Total != 3 || len(page.Entries) != 1 || page.Entries[0].Path != "b/two.mkv" {
		t.Errorf("second page of one: total %d, %+v", page.Total, page.Entries)
	}

	if none := m.dryRunPage(t, "&op=delete"); none.Total != 0 {
		t.Errorf("op=delete matched %d", none.Total)
	}
	if found := m.dryRunPage(t, "&q=THREE"); found.Total != 1 {
		t.Errorf("q=THREE matched %d, want 1", found.Total)
	}
}
