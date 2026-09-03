package app

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"blackforestbytes.com/jcc-mirror/store"
)

// mirror is a whole publisher behind a real WebDAV server, plus a destination
// directory. The dashboard's runs go through exactly the code the daemon uses -
// App.Remote returns a WebDAV client and nothing here fakes one.
type mirror struct {
	app   *App
	h     http.Handler
	token string
	src   string
	dst   string
	pair  store.Pair
}

func newMirror(t *testing.T) *mirror {
	t.Helper()
	a, h, token := newApp(t)

	src, dst := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(&xwebdav.Handler{FileSystem: xwebdav.Dir(src), LockSystem: xwebdav.NewMemLS()})
	t.Cleanup(srv.Close)

	// The reserve is set to nothing: the default keeps 50 GiB free on the
	// destination volume, and whether the machine running the tests has that much
	// is not something these tests are about.
	form := url.Values{store.KeyRemoteURL: {srv.URL}, store.KeyReserve: {"1"}}
	if rec := postForm(t, h, "/api/config", token, form); rec.Code != http.StatusOK {
		t.Fatalf("configure the remote: %d %s", rec.Code, rec.Body)
	}

	m := &mirror{app: a, h: h, token: token, src: src, dst: dst}
	m.pair = m.addPair(t, url.Values{
		"name":      {"media"},
		"localPath": {dst},
		"enabled":   {"true"},
	})
	return m
}

func (m *mirror) addPair(t *testing.T, form url.Values) store.Pair {
	t.Helper()
	if rec := postForm(t, m.h, "/api/pairs", m.token, form); rec.Code != http.StatusOK {
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

// run starts an operation and waits for it to finish, which is what an operator
// does by watching the page refresh itself.
func (m *mirror) run(t *testing.T, kind string) Run {
	t.Helper()

	form := url.Values{"kind": {kind}, "pair": {strconv.FormatInt(m.pair.ID, 10)}}
	rec := postForm(t, m.h, "/api/runs", m.token, form)
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

	// The second plan is the one that matters: it proves the round trip through a
	// real WebDAV server preserved what the differ compares on.
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
	if rec := postForm(t, m.h, "/api/runs", m.token, form); rec.Code != http.StatusAccepted {
		t.Fatalf("first run: %d %s", rec.Code, rec.Body)
	}

	// Whichever way this races, the answer has to be either "started" or a clear
	// refusal - never two walks writing the same manifest.
	rec := postForm(t, m.h, "/api/runs", m.token, form)
	if rec.Code != http.StatusConflict && rec.Code != http.StatusAccepted {
		t.Fatalf("second run: %d %s", rec.Code, rec.Body)
	}
	if rec.Code == http.StatusConflict && !strings.Contains(rec.Body.String(), "running") {
		t.Errorf("the refusal does not say why: %s", rec.Body)
	}
}

func TestRunAndPairEndpointsNeedTheToken(t *testing.T) {
	m := newMirror(t)
	for _, path := range []string{"/api/pairs", "/api/pairs/update", "/api/pairs/delete", "/api/runs", "/api/runs/cancel"} {
		if rec := postForm(t, m.h, path, "", url.Values{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token = %d, want 401", path, rec.Code)
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
	if rec := postForm(t, m.h, "/api/pairs/update", m.token, form); rec.Code != http.StatusOK {
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
	rec := postForm(t, m.h, "/api/runs", m.token,
		url.Values{"kind": {RunScan}, "pair": {strconv.FormatInt(m.pair.ID, 10)}})
	if rec.Code != http.StatusConflict {
		t.Errorf("running a disabled pair = %d, want 409", rec.Code)
	}

	if rec := postForm(t, m.h, "/api/pairs/delete", m.token, url.Values{"id": {strconv.FormatInt(m.pair.ID, 10)}}); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if pairs, err := m.app.store.Pairs(context.Background()); err != nil || len(pairs) != 0 {
		t.Errorf("after the delete there are %d pairs (err %v)", len(pairs), err)
	}
}

// TestSetupPageShowsTheMirror renders the page the operator actually looks at.
func TestSetupPageShowsTheMirror(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 1024)
	m.run(t, RunScan)

	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{"Mirror", "media", m.dst, `name="kind" value="adopt"`, "Recent runs"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
	// Nothing is running, so the page must not ask the browser to reload itself.
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the page refreshes itself while nothing is running")
	}
}

// TestSetupPageRefreshesWhileBusy: the view has no script, so the meta refresh is
// the only thing that makes a running transfer's progress move. Without it an
// operator watches a frozen page for a day and a half.
func TestSetupPageRefreshesWhileBusy(t *testing.T) {
	m := newMirror(t)

	started := time.Now()
	m.app.runs.mu.Lock()
	m.app.runs.current = &Run{Kind: RunSync, PairID: m.pair.ID, PairName: m.pair.Name, StartedAt: started}
	m.app.runs.mu.Unlock()

	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("a running operation does not make the page refresh itself")
	}
	if !strings.Contains(body, "Stop") {
		t.Error("a running operation cannot be stopped from the page")
	}
}
