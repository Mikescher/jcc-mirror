package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
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

// mirror is a whole publisher's tree, plus the destination directory it is
// mirrored into. The dashboard's runs go through exactly the code the daemon
// uses; only what is behind App.Remote is local, because SMB has no in-process
// server to stand one up with.
type mirror struct {
	app  *App
	h    *dash
	src  string
	dst  string
	pair store.Pair
}

func newMirror(t *testing.T) *mirror {
	t.Helper()

	src, dst := t.TempDir(), t.TempDir()
	a, h := newAppWithRemote(t, src)

	// The reserve is set to nothing: the default keeps 50 GiB free on the
	// destination volume, and whether the machine running the tests has that much
	// is not something these tests are about.
	form := url.Values{store.KeyReserve: {"1"}}
	if rec := postForm(t, h, "/api/config", form); rec.Code != http.StatusOK {
		t.Fatalf("configure the remote: %d %s", rec.Code, rec.Body)
	}

	m := &mirror{app: a, h: h, src: src, dst: dst}
	m.pair = m.addPair(t, url.Values{
		"name":      {"media"},
		"localPath": {dst},
		"enabled":   {"true"},
	})
	return m
}

func (m *mirror) addPair(t *testing.T, form url.Values) store.Pair {
	t.Helper()
	if rec := postForm(t, m.h, "/api/pairs", form); rec.Code != http.StatusOK {
		t.Fatalf("add pair: %d %s", rec.Code, rec.Body)
	}
	pairs, err := m.app.store.Pairs(context.Background())
	if err != nil || len(pairs) == 0 {
		t.Fatalf("pairs after add: %v (%d)", err, len(pairs))
	}
	return pairs[len(pairs)-1]
}

func (m *mirror) write(t *testing.T, rel string, size int) []byte {
	t.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand: %v", err)
	}
	full := filepath.Join(m.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return body
}

// updatePair changes the pair through the same endpoint the form posts to, so a
// test cannot set a field the dashboard cannot.
func (m *mirror) updatePair(t *testing.T, form url.Values) {
	t.Helper()

	form.Set("id", strconv.FormatInt(m.pair.ID, 10))
	if rec := postForm(t, m.h, "/api/pairs/update", form); rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("update pair: %d %s", rec.Code, rec.Body)
	}

	pair, err := m.app.store.PairByID(context.Background(), m.pair.ID)
	if err != nil {
		t.Fatalf("reload pair: %v", err)
	}
	m.pair = pair
}

// run starts an operation and waits for it to finish, which is what an operator
// does by watching the page refresh itself.
func (m *mirror) run(t *testing.T, kind string) Run {
	t.Helper()

	form := url.Values{"kind": {kind}, "pair": {strconv.FormatInt(m.pair.ID, 10)}}
	rec := postForm(t, m.h, "/api/runs", form)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start %s: %d %s", kind, rec.Code, rec.Body)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state := m.app.Runs(context.Background())
		if state.Current == nil && len(state.History) > 0 {
			return state.History[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not finish", kind)
	return Run{}
}

// TestDashboardRunsTheWholeMirror is the reason these buttons exist: the whole
// bootstrap has to be reachable from a browser, because the container's shell on
// a Synology is not somewhere an operator can easily get to.
func TestDashboardRunsTheWholeMirror(t *testing.T) {
	m := newMirror(t)
	want := map[string][]byte{
		"Filme/Grüße.mkv":    m.write(t, "Filme/Grüße.mkv", 40*1024),
		"Serien/S01/e01.mkv": m.write(t, "Serien/S01/e01.mkv", 2048),
	}

	if scan := m.run(t, RunScan); scan.Error != "" {
		t.Fatalf("scan: %s", scan.Error)
	}

	plan := m.run(t, RunPlan)
	if plan.Error != "" || plan.Plan == nil {
		t.Fatalf("plan: %s", plan.Error)
	}
	if plan.Plan.Add != len(want) {
		t.Errorf("plan wants %d adds, want %d", plan.Plan.Add, len(want))
	}

	sync := m.run(t, RunSync)
	if sync.Error != "" {
		t.Fatalf("sync: %s", sync.Error)
	}
	for rel, body := range want {
		got, err := os.ReadFile(filepath.Join(m.dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != string(body) {
			t.Errorf("%s came across wrong", rel)
		}
	}

	// The second plan is the one that matters: it proves the round trip preserved
	// what the differ compares on.
	if after := m.run(t, RunPlan); after.Plan.Transfers() != 0 {
		t.Errorf("a second plan still wants %d transfers", after.Plan.Transfers())
	}
}

// TestDashboardAdoptsABootstrap is the case the buttons were added for: the USB
// copy is already on the Synology and has to be recognised without a shell.
func TestDashboardAdoptsABootstrap(t *testing.T) {
	m := newMirror(t)
	body := m.write(t, "Filme/Big.mkv", 8192)

	// Copied by hand, with a timestamp the copy did not preserve.
	full := filepath.Join(m.dst, "Filme", "Big.mkv")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	m.run(t, RunScan)
	adopt := m.run(t, RunAdopt)
	if adopt.Error != "" {
		t.Fatalf("adopt: %s", adopt.Error)
	}
	if !strings.Contains(adopt.Summary, "1 files matched") {
		t.Errorf("adopt summary = %q", adopt.Summary)
	}

	if plan := m.run(t, RunPlan); plan.Plan.Transfers() != 0 {
		t.Errorf("after adopting, a plan still wants %d transfers - the bootstrap would go down the wire again",
			plan.Plan.Transfers())
	}
}

func TestOnlyOneRunAtATime(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 1024)

	form := url.Values{"kind": {RunScan}, "pair": {strconv.FormatInt(m.pair.ID, 10)}}
	if rec := postForm(t, m.h, "/api/runs", form); rec.Code != http.StatusAccepted {
		t.Fatalf("first run: %d %s", rec.Code, rec.Body)
	}

	// Whichever way this races, the answer has to be either "started" or a clear
	// refusal - never two walks writing the same manifest.
	rec := postForm(t, m.h, "/api/runs", form)
	if rec.Code != http.StatusConflict && rec.Code != http.StatusAccepted {
		t.Fatalf("second run: %d %s", rec.Code, rec.Body)
	}
	if rec.Code == http.StatusConflict && !strings.Contains(rec.Body.String(), "running") {
		t.Errorf("the refusal does not say why: %s", rec.Body)
	}
}

// TestRunAndPairEndpointsAnswerAboutTheRequest: past the password, an action
// posted with nothing in it is answered about what is missing from the request
// rather than about the caller.
func TestRunAndPairEndpointsAnswerAboutTheRequest(t *testing.T) {
	m := newMirror(t)
	for _, path := range []string{"/api/pairs", "/api/pairs/update", "/api/pairs/delete", "/api/runs", "/api/runs/cancel"} {
		rec := postForm(t, m.h, path, url.Values{})
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
}

func TestPairEditingThroughTheDashboard(t *testing.T) {
	m := newMirror(t)

	form := url.Values{
		"id":       {strconv.FormatInt(m.pair.ID, 10)},
		"name":     {"films"},
		"excludes": {"**/*.tmp, Trash/**"},
		"enabled":  {"false"},
	}
	if rec := postForm(t, m.h, "/api/pairs/update", form); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	p, err := m.app.store.PairByID(context.Background(), m.pair.ID)
	if err != nil {
		t.Fatalf("PairByID: %v", err)
	}
	if p.Name != "films" || p.Enabled {
		t.Errorf("after the edit the pair is %+v", p)
	}
	if len(p.Excludes) != 2 || p.Excludes[0] != "**/*.tmp" {
		t.Errorf("excludes = %q, want the two globs split on the comma", p.Excludes)
	}
	// Untouched fields keep their value: the form posts everything, but a JSON
	// caller sending one key must not blank the rest.
	if p.LocalPath != m.dst {
		t.Errorf("local path = %q, want it unchanged", p.LocalPath)
	}

	// A disabled pair is not something to start a run on by accident.
	rec := postForm(t, m.h, "/api/runs",
		url.Values{"kind": {RunScan}, "pair": {strconv.FormatInt(m.pair.ID, 10)}})
	if rec.Code != http.StatusConflict {
		t.Errorf("running a disabled pair = %d, want 409", rec.Code)
	}

	if rec := postForm(t, m.h, "/api/pairs/delete", url.Values{"id": {strconv.FormatInt(m.pair.ID, 10)}}); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if pairs, err := m.app.store.Pairs(context.Background()); err != nil || len(pairs) != 0 {
		t.Errorf("after the delete there are %d pairs (err %v)", len(pairs), err)
	}
}

// TestPairsViewShowsTheMirror is what the Now view is drawn from: one request
// that says, per pair, what the publisher has and what is here.
func TestPairsViewShowsTheMirror(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 1024)
	m.run(t, RunScan)

	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/pairs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/pairs = %d: %s", rec.Code, rec.Body)
	}

	var views []PairView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d pairs, want 1", len(views))
	}

	got := views[0]
	if got.Name != "media" || got.LocalPath != m.dst {
		t.Errorf("pair = %q at %q, want media at %q", got.Name, got.LocalPath, m.dst)
	}
	if got.RemoteFiles != 1 || got.RemoteBytes != 1024 {
		t.Errorf("the walk is reported as %d files (%d bytes), want 1 (1024)", got.RemoteFiles, got.RemoteBytes)
	}
	if got.BehindFiles != 1 || got.BehindBytes != 1024 {
		t.Errorf("behind = %d files (%d bytes), want 1 (1024): nothing has been transferred yet", got.BehindFiles, got.BehindBytes)
	}
	if got.LastScan == nil {
		t.Error("the completed walk is not reported")
	}
}

// TestStreamCarriesTheRunningOperation is what replaced the meta refresh of the
// old setup page: without it an operator watches a frozen page for a day and a
// half (DESIGN.md §4).
func TestStreamCarriesTheRunningOperation(t *testing.T) {
	m := newMirror(t)

	m.app.runs.mu.Lock()
	m.app.runs.current = &Run{Kind: RunSync, PairID: m.pair.ID, PairName: m.pair.Name, StartedAt: time.Now()}
	m.app.runs.mu.Unlock()

	srv := httptest.NewServer(m.h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.h.password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	frame := readFrame(t, res.Body)
	if !strings.HasPrefix(frame, "event: state") {
		t.Fatalf("the first frame is %q, want the state", frame)
	}

	var state StreamState
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frame[strings.Index(frame, "data: ")+len("data: "):], "")), &state); err != nil {
		t.Fatalf("decode the state frame: %v", err)
	}
	if state.Runs.Current == nil || state.Runs.Current.Kind != RunSync {
		t.Fatalf("the state frame does not carry the running sync: %+v", state.Runs.Current)
	}
}

// readFrame reads one server-sent event, which ends at the blank line.
func readFrame(t *testing.T, body io.Reader) string {
	t.Helper()

	reader := bufio.NewReader(body)
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		if line == "\n" {
			return frame.String()
		}
		frame.WriteString(line)
	}
}

// TestDashboardApprovesADeletion is §2.5 from the operator's side: a mirror pair
// that wants to delete more than its guard allows stops, says so on the page, and
// goes ahead only once someone has pressed the button.
func TestDashboardApprovesADeletion(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/gone.mkv", 2048)
	m.write(t, "Filme/stays.mkv", 1024)

	// No per-pair count limit at all, so what holds this back is the stored 10%
	// threshold: one of two files is half the pair.
	m.updatePair(t, url.Values{"mode": {store.ModeGuarded}, "deleteGuard": {"0"}})
	m.run(t, RunScan)
	if sync := m.run(t, RunSync); sync.Error != "" {
		t.Fatalf("sync: %s", sync.Error)
	}

	if err := os.Remove(filepath.Join(m.src, "Filme", "gone.mkv")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m.run(t, RunScan)

	held := m.run(t, RunSync)
	if held.Error != "" {
		t.Fatalf("sync: %s", held.Error)
	}
	if !strings.Contains(held.Summary, "waiting for approval") {
		t.Errorf("sync summary = %q, want it to say the deletion is held", held.Summary)
	}
	if _, err := os.Stat(filepath.Join(m.dst, "Filme", "gone.mkv")); err != nil {
		t.Fatalf("the held file is gone from the tree: %v", err)
	}

	views, err := m.app.PairViews(context.Background())
	if err != nil {
		t.Fatalf("pair views: %v", err)
	}
	if views[0].Approval == nil || !views[0].Approval.Pending() {
		t.Fatalf("the page shows %+v, want a pending approval", views[0].Approval)
	}

	// And it says so where the operator is, not only in the event log: a mirror
	// that has quietly stopped deleting is the failure this guard trades for.
	page := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/pairs", nil))
	var shown []PairView
	if err := json.Unmarshal(page.Body.Bytes(), &shown); err != nil {
		t.Fatalf("decode /api/pairs: %v", err)
	}
	if len(shown) != 1 || shown[0].Approval == nil || !shown[0].Approval.Pending() {
		t.Errorf("the pairs view does not surface the held deletion: %s", page.Body)
	}

	form := url.Values{"id": {strconv.FormatInt(m.pair.ID, 10)}, "decision": {store.ApprovalApproved}}
	if rec := postForm(t, m.h, "/api/pairs/deletions", form); rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}

	done := m.run(t, RunDelete)
	if done.Error != "" {
		t.Fatalf("delete: %s", done.Error)
	}
	if _, err := os.Stat(filepath.Join(m.dst, "Filme", "gone.mkv")); err == nil {
		t.Error("the approved file is still in the tree")
	}
	if _, err := os.Stat(filepath.Join(m.dst, "Filme", "stays.mkv")); err != nil {
		t.Errorf("a file the publisher still has was deleted: %v", err)
	}
}
